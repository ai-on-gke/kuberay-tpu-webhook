/**
 * Copyright 2024 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"math"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	ray "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// slice represents a TPU Pod Slice.
type slice struct {
	clusterName  string
	groupName    string
	namespace    string
	replicaIndex int
	numOfHosts   int32
}

// TPUWebhookServer is a KubeRay TPU webhook server instance.
type TPUWebhookServer struct {
	// podLister is used to query Pods from an informer cache.
	podLister listersv1.PodLister
	// nodeLister is used to query Nodes from an informer cache.
	nodeLister listersv1.NodeLister
	cacheMutex sync.Mutex
	cacheCond  *podSyncCond
}

// patch is a JSON patch describing mutate operation(s) for an incoming object.
type patch map[string]any

const (
	// TLS certificate related constants.
	certPath = "/etc/kuberay-tpu-webhook/tls/tls.crt"
	keyPath  = "/etc/kuberay-tpu-webhook/tls/tls.key"

	// TPU related constants.
	tpuProcessPortBase = 8471
	megascalePortBase  = 8081
	tpuResourceName    = corev1.ResourceName("google.com/tpu")
	tpu7xType          = "tpu7x"

	// GKE label prefix.
	gkeLabelPrefix = "cloud.google.com/"

	tpuTopologyLabel              = gkeLabelPrefix + "gke-tpu-topology"
	tpuSubsliceTopologyAnnotation = gkeLabelPrefix + "gke-tpu-slice-topology"
	legacyReplicaIndexLabelKey    = "replicaIndex"

	// GKE labels.
	gkeTPUAcceleratorLabel = gkeLabelPrefix + "gke-tpu-accelerator"
	gkeNodePoolLabel       = gkeLabelPrefix + "gke-nodepool"

	// Topology labels.
	gceTopologyBlockLabel    = gkeLabelPrefix + "gce-topology-block"
	gceTopologySubblockLabel = gkeLabelPrefix + "gce-topology-subblock"
	gceTopologyHostLabel     = gkeLabelPrefix + "gce-topology-host"
)

var (
	// Flag arguments.
	BindAddr       string
	KubeConfigPath string
	ServerCert     string
	ServerKey      string

	CertFile string
	KeyFile  string
)

func NewTPUWebhookServer(podLister listersv1.PodLister, nodeLister listersv1.NodeLister) *TPUWebhookServer {
	return &TPUWebhookServer{
		podLister:  podLister,
		nodeLister: nodeLister,
		cacheCond:  newPodSyncCond(),
	}
}

// Mutate handles http Request for Pod creation and writes a response
func (t *TPUWebhookServer) Mutate(w http.ResponseWriter, r *http.Request) {
	admissionReview := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(admissionReview); err != nil {
		http.Error(w, "Error decoding request body", http.StatusBadRequest)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil || admissionReview.Request.Kind.Kind != "Pod" {
		http.Error(w, "Invalid Kind", http.StatusBadRequest)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	klog.V(0).InfoS("Mutate", "Received review for Pod creation with name", admissionReview.Request.Name)
	response, err := t.mutatePod(admissionReview)
	if err != nil {
		klog.Errorf("Failed to mutate Pod: %s", err)
		http.Error(w, "Failed to mutate Pod", http.StatusForbidden)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	admissionReview.Response = response
	responseBytes, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Failed to encode response: %s", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(responseBytes))
}

// Validate handles http Request for RayCluster creation and writes a response
func (t *TPUWebhookServer) Validate(w http.ResponseWriter, r *http.Request) {
	admissionReview := &admissionv1.AdmissionReview{}
	if err := json.NewDecoder(r.Body).Decode(admissionReview); err != nil {
		http.Error(w, "Error decoding request body", http.StatusBadRequest)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if admissionReview.Request == nil || admissionReview.Request.Kind.Kind != "RayCluster" {
		http.Error(w, "Invalid Kind", http.StatusBadRequest)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	klog.V(0).InfoS("Validate", "Received review for RayCluster creation with name", admissionReview.Request.Name)
	response, err := t.validateRayCluster(admissionReview)
	if err != nil {
		klog.Errorf("Failed to validate RayCluster: %s", err)
		http.Error(w, "Failed to validate RayCluster", http.StatusForbidden)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	admissionReview.Response = response
	responseBytes, err := json.Marshal(admissionReview)
	if err != nil {
		klog.Errorf("Failed to encode response: %s", err)
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(responseBytes))
}

// printsliceToTPUHosts logs sliceToTPUHosts contents for debugging
func printsliceToTPUHosts(sliceToTPUHosts map[slice][]int) {
	for slice, workerList := range sliceToTPUHosts {
		klog.V(1).InfoS("printsliceToTPUHosts", "RayCluster", slice.namespace+"/"+slice.clusterName, "Worker Group", slice.groupName)
		for _, workerID := range workerList {
			klog.V(1).InfoS("printsliceToTPUHosts", "RayCluster", slice.namespace+"/"+slice.clusterName, "Worker ID", workerID)
		}
	}
}

// containerRequestingTPUs returns whether containers are requesting TPU resources
func containerRequestingTPUs(containers ...corev1.Container) bool {
	for _, container := range containers {
		if l := container.Resources.Limits; l != nil {
			if resource := l[tpuResourceName]; !resource.IsZero() {
				return true
			}
		}
		if r := container.Resources.Requests; r != nil {
			if resource := r[tpuResourceName]; !resource.IsZero() {
				return true
			}
		}
	}
	return false
}

// getNumTPUChipsRequested returns the total `google.com/TPU` Resource request value
// summed across all containers. This indicates the total number of TPU chips the Pod requires.
func getNumTPUChipsRequested(containers ...corev1.Container) int64 {
	totalTPUs := int64(0)
	for _, container := range containers {
		containerTPU := int64(0)
		hasRequest := false

		if r := container.Resources.Requests; r != nil {
			if resource := r[tpuResourceName]; !resource.IsZero() {
				containerTPU = resource.Value()
				hasRequest = true
			}
		}
		if !hasRequest {
			// default to limit if request is omitted
			if l := container.Resources.Limits; l != nil {
				if resource := l[tpuResourceName]; !resource.IsZero() {
					containerTPU = resource.Value()
				}
			}
		}
		totalTPUs += containerTPU
	}
	return totalTPUs
}

// getNumTPUHostsFromTopology returns number of TPU VM hosts in Pod Slice specified by gke-tpu-topology Pod nodeSelector
func getNumTPUHostsFromTopology(clusterName string, groupName string, namespace string, topology string, chipsPerHost int64) (int32, error) {
	if topology == "" {
		return 0, errors.New("TPU topology not specified")
	}
	topologyVals := strings.Split(topology, "x")
	chips := 1
	for i := 0; i < len(topologyVals); i++ {
		dim, err := strconv.Atoi(topologyVals[i])
		if err != nil {
			klog.ErrorS(err, "getNumTPUHostsFromTopology", "RayCluster", namespace+"/"+clusterName, "Worker Group", groupName, "gke-tpu-topology", topology)
			return 0, err
		}
		chips *= dim
	}
	// calculate the # of VMs using # of chips per host
	hosts := max(int32(chips)/int32(chipsPerHost), 1)
	klog.V(1).InfoS("getNumTPUHostsFromTopology", "RayCluster", namespace+"/"+clusterName, "Worker Group", groupName, "topology", topology, "chips", chips, "hosts", hosts)
	return hosts, nil
}

// extractRayCluster returns RayCluster unmarshalled from an admission request
func extractRayCluster(admissionReview *admissionv1.AdmissionReview) (*ray.RayCluster, error) {
	if admissionReview.Request.Kind.Kind != "RayCluster" {
		return nil, fmt.Errorf("Expected RayCluster but got %s", admissionReview.Request.Kind.Kind)
	}

	rayCluster := ray.RayCluster{}
	if err := json.Unmarshal(admissionReview.Request.Object.Raw, &rayCluster); err != nil {
		return nil, err
	}

	return &rayCluster, nil
}

// generateHeadlessServiceName returns the expected TPU headless service name for a RayCluster
func generateHeadlessServiceName(clusterName string) string {
	serviceName := fmt.Sprintf("%s-%s", clusterName, utils.HeadlessServiceSuffix)

	// Apply the same truncation as in the RayCluster controller when generating the headless service
	// name. This is to maintain the up-to 63 char compatibility guarantee for hostnames (RFC 1123).
	return utils.CheckName(serviceName)
}

// genDNSHostnames returns list of DNS hostnames for TPU VM hosts as a string
func genDNSHostnames(numOfHosts int32, groupName string, clusterName string, namespace string, replicaIndex int) (string, error) {
	if numOfHosts == 0 {
		err := errors.New("workerGroupSpec NumOfHosts not set")
		return "", err
	}
	headlessServiceName := generateHeadlessServiceName(clusterName)
	hostNames := make([]string, numOfHosts)
	// Host names will be of the form {WORKER_GROUP_NAME}-{REPLICA_INDEX}-{HOST_INDEX}.{CLUSTER_NAME}-headless
	for j := 0; j < int(numOfHosts); j++ {
		hostNames[j] = fmt.Sprintf("%s-%d-%d.%s", groupName, replicaIndex, j, headlessServiceName)
	}
	klog.V(1).InfoS("genDNSHostnames", "RayCluster", namespace+"/"+clusterName, "NumOfHosts", numOfHosts, "Replica Index", replicaIndex)
	return strings.Join(hostNames, ","), nil
}

// getTPUProcessAddresses returns the list of process addresses (host:port) for all containers in the slice.
// The base port is 8476. TPU_PROCESS_ADDRESSES replaces TPU_WORKER_HOSTNAMES for Ironwood (v7x) TPUs and newer.
func getTPUProcessAddresses(numOfHosts int32, numTpuContainers int, groupName, clusterName string, replicaIndex int) (string, error) {
	if numOfHosts == 0 {
		return "", errors.New("workerGroupSpec NumOfHosts not set")
	}
	headlessServiceName := generateHeadlessServiceName(clusterName)
	var addresses []string

	for h := 0; h < int(numOfHosts); h++ {
		hostName := fmt.Sprintf("%s-%d-%d.%s", groupName, replicaIndex, h, headlessServiceName)
		for c := 0; c < numTpuContainers; c++ {
			port := tpuProcessPortBase + c
			addresses = append(addresses, fmt.Sprintf("%s:%d", hostName, port))
		}
	}
	return strings.Join(addresses, ","), nil
}

// injectHostnames TPU_WORKER_HOSTNAMES into a Pod for multi-host initialization.
func injectHostnames(clusterName string, hostNames string, envPath string, container corev1.Container, patches *[]patch, envArrayExists bool) bool {
	tpuWorkerHostNames := corev1.EnvVar{
		Name:  "TPU_WORKER_HOSTNAMES",
		Value: hostNames,
	}
	// Append hostnames patch to the current list of patches.
	var updatedPatches []patch
	updatedPatches, envArrayExists = addEnvVarPatch(*patches, tpuWorkerHostNames, envPath, envArrayExists)
	*patches = updatedPatches

	return envArrayExists
}

// injectSubdomain sets the Pod subdomain to match the headless service.
func injectSubdomain(clusterName string, patches *[]patch) {
	subdomainPatch := patch{
		"op":    "add",
		"path":  "/spec/subdomain",
		"value": generateHeadlessServiceName(clusterName),
	}
	*patches = append(*patches, subdomainPatch)
}

// injectReplicaLabel injects replicaIndex label into a Pod for TPU Pod scheduling and Ray multi-host autoscaling
func injectReplicaLabel(clusterName string, namespace string, replicaIndex int, workerGroupName string, patches *[]patch) {
	labelPatch := patch{"op": "add"}
	labelPath := "/metadata/labels/replicaIndex"
	replicaLabelValue := fmt.Sprintf("%s-%d", workerGroupName, replicaIndex)

	klog.V(1).InfoS("injectReplicaLabel", "RayCluster", namespace+"/"+clusterName, legacyReplicaIndexLabelKey, replicaLabelValue)

	labelPatch["path"] = labelPath
	labelPatch["value"] = replicaLabelValue

	*patches = append(*patches, labelPatch)
}

// makeLabelSelectorRequirement is a helper function to create a LabelSelectorRequirement
// with a given key, operator, and values.
func makeLabelSelectorRequirement(key string, op metav1.LabelSelectorOperator, values ...string) metav1.LabelSelectorRequirement {
	return metav1.LabelSelectorRequirement{
		Key:      key,
		Operator: op,
		Values:   values,
	}
}

// getGKETopologyKey returns the first GKE topology key found in the Pod's affinity,
// defaulting to the nodepool key if none are found.
func getGKETopologyKey(pod *corev1.Pod) string {
	if pod.Spec.Affinity != nil && pod.Spec.Affinity.PodAffinity != nil {
		for _, term := range pod.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			if strings.HasPrefix(term.TopologyKey, gkeLabelPrefix) {
				return term.TopologyKey
			}
		}
	}
	return gkeNodePoolLabel
}

// injectAffinity injects pod affinity and anti-affinity scheduling constraints using replicaIndex and cluster labels
// to ensure TPU Pods from the same multi-host replica are co-located.
func (t *TPUWebhookServer) injectAffinity(pod *corev1.Pod, replicaIndex int, numOfHosts int, workerGroupName string, patches *[]patch) error {
	clusterName := pod.Labels[utils.RayClusterLabelKey]
	topologyKey := getGKETopologyKey(pod)

	// If the current topology key is the default nodepool, attempt to discover a more specific one (block/subblock).
	if _, subsliceRequested := pod.Annotations[tpuSubsliceTopologyAnnotation]; subsliceRequested {
		selector := labels.SelectorFromSet(pod.Spec.NodeSelector)
		nodes, err := t.nodeLister.List(selector)
		if err == nil && len(nodes) > 0 {
			topology := buildTPUTopology(nodes)
			if key, err := topology.subsliceAffinityKey(numOfHosts); err == nil {
				topologyKey = key
			} else {
				// If no topologyKey could be identified despite nodes being
				// available, the pod should be rejected with an error.
				return fmt.Errorf("schedule pod for subslice on %d possible nodes not possible: %w", len(nodes), err)
			}
		} else if err != nil {
			klog.V(0).ErrorS(err, "Cannot choose subslice affinity key, list nodes returned err")
			return err
		} else if len(nodes) == 0 {
			klog.V(0).Info("Cannot choose subslice affinity key, no nodes exist for choose subslice affinity")
			// This is not an error because the pod may need to be admitted to
			// trigger scale up.
			// It will almost certainly be scheduled incorrectly, but the
			// RayCluster admission should have already warned the user.
		}
	}

	var affinityLabelKey string
	var affinityLabelValue string

	// Check if the Pod has native KubeRay indexing labels
	replicaName, hasReplicaNameLabel := pod.Labels[utils.RayWorkerReplicaNameKey]
	_, hasHostLabel := pod.Labels[utils.RayHostIndexKey]

	// Determine the label to use for affinity, set either by KubeRay or this webhook.
	if hasReplicaNameLabel && hasHostLabel {
		// Use the unique replica name (e.g. ray.io/worker-group-replica-name: workergroup-xh3hf) for scheduling constraints
		affinityLabelKey = utils.RayWorkerReplicaNameKey
		affinityLabelValue = replicaName
	} else {
		// Fallback to legacy webhook label (e.g. replicaIndex: workergroup-0)
		affinityLabelKey = legacyReplicaIndexLabelKey
		affinityLabelValue = fmt.Sprintf("%s-%d", workerGroupName, replicaIndex)
	}

	// Co-schedule on a node-pool Pods with the same unique replica name and RayCluster
	replicaIn := makeLabelSelectorRequirement(affinityLabelKey, metav1.LabelSelectorOpIn, affinityLabelValue)
	clusterIn := makeLabelSelectorRequirement(utils.RayClusterLabelKey, metav1.LabelSelectorOpIn, clusterName)
	podAffinityTerm := corev1.PodAffinityTerm{
		LabelSelector: &metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{replicaIn, clusterIn},
		},
		TopologyKey: topologyKey,
	}

	// Avoid scheduling on a node-pool with Pods of a different replica name label
	replicaNotIn := makeLabelSelectorRequirement(affinityLabelKey, metav1.LabelSelectorOpNotIn, affinityLabelValue)
	clusterNotIn := makeLabelSelectorRequirement(utils.RayClusterLabelKey, metav1.LabelSelectorOpNotIn, clusterName)
	replicaExists := makeLabelSelectorRequirement(affinityLabelKey, metav1.LabelSelectorOpExists)

	podAntiAffinityTerms := []corev1.PodAffinityTerm{
		{
			// Repel pods in the same cluster that have a different replica index.
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					replicaNotIn,
					clusterIn,
				},
			},
			TopologyKey: topologyKey,
		},
		{
			// Repel Pods from different RayClusters from scheduling to this node pool.
			LabelSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					clusterNotIn,
					replicaExists,
				},
			},
			TopologyKey: topologyKey,
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "kubernetes.io/metadata.name",
						Operator: metav1.LabelSelectorOpNotIn,
						Values:   []string{"kube-system"}, // match all except kube-system
					},
				},
			},
		},
	}

	// Incorporate existing affinity if present.
	combinedAffinity := corev1.Affinity{}
	if pod.Spec.Affinity != nil {
		combinedAffinity = *pod.Spec.Affinity.DeepCopy()
	}

	if combinedAffinity.PodAffinity == nil {
		combinedAffinity.PodAffinity = &corev1.PodAffinity{}
	}
	combinedAffinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		combinedAffinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		podAffinityTerm,
	)

	if combinedAffinity.PodAntiAffinity == nil {
		combinedAffinity.PodAntiAffinity = &corev1.PodAntiAffinity{}
	}
	combinedAffinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
		combinedAffinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		podAntiAffinityTerms...,
	)

	*patches = append(*patches, patch{
		"op":    "add",
		"path":  "/spec/affinity",
		"value": combinedAffinity,
	})
	return nil
}

// checkWorkersMatchTopology returns whether the # of Ray TPU worker pods equals the # of hosts defined in the topology key
func checkWorkersMatchTopology(clusterName string, namespace string, workerGroupSpec ray.WorkerGroupSpec) (bool, error) {
	klog.V(1).InfoS("checkWorkersMatchTopology", "RayCluster", namespace+"/"+clusterName, "workerGroup", workerGroupSpec.GroupName)
	numHosts := workerGroupSpec.NumOfHosts // 1 TPU VM host -> 1 Ray worker pod
	if numHosts == 0 {
		return false, errors.New("workerGroupSpec NumOfHosts not set")
	}
	groupName := workerGroupSpec.GroupName
	containers := workerGroupSpec.Template.Spec.Containers
	if len(containers) == 0 {
		return false, errors.New("Container path not specified")
	}
	if containerRequestingTPUs(containers...) {
		topology, ok := workerGroupSpec.Template.Annotations[tpuSubsliceTopologyAnnotation]
		if !ok {
			topology = workerGroupSpec.Template.Spec.NodeSelector[tpuTopologyLabel]
		}
		klog.V(0).InfoS("checkWorkersMatchTopology", "RayCluster", namespace+"/"+clusterName, "topology", topology, "NumOfHosts", numHosts)
		if topology == "" {
			err := errors.New("TPU topology not specified")
			klog.ErrorS(err, "checkWorkersMatchTopology", "RayCluster", namespace+"/"+clusterName, "topology", topology)
			return false, err
		}
		chipsPerHost := getNumTPUChipsRequested(containers...)
		if chipsPerHost == 0 {
			err := errors.New("Container does not set TPU limits")
			klog.ErrorS(err, "checkWorkersMatchTopology", "RayCluster", namespace+"/"+clusterName, "topology", topology)
			return false, err
		}
		expectedHosts, err := getNumTPUHostsFromTopology(clusterName, groupName, namespace, topology, chipsPerHost)
		if err != nil {
			return false, err
		}

		if expectedHosts != numHosts {
			return false, nil
		}
	}
	return true, nil
}

// validateRayCluster returns an Admission Response after checking Ray worker groups match TPU scheduling constraints
func (t *TPUWebhookServer) validateRayCluster(admissionReview *admissionv1.AdmissionReview) (*admissionv1.AdmissionResponse, error) {
	raycluster, err := extractRayCluster(admissionReview)
	if err != nil {
		return nil, err
	}

	admit := true
	status := "Success"
	message := ""
	var warnings []string
	clusterName := raycluster.Name
	if clusterName == "" {
		clusterName = admissionReview.Request.Name
	}
	namespace := raycluster.Namespace
	klog.V(1).InfoS("validateRayCluster", "RayCluster", namespace+"/"+clusterName)
	workerGroupSpecs := raycluster.Spec.WorkerGroupSpecs
	for i := 0; i < len(workerGroupSpecs); i++ {
		workerGroupSpec := workerGroupSpecs[i]
		workerGroupContainers := workerGroupSpec.Template.Spec.Containers
		if len(workerGroupContainers) != 0 && !containerRequestingTPUs(workerGroupContainers...) {
			// pass through if no TPUs are requested
			continue
		}
		// validate NumOfHosts for worker group matches topology nodeSelector
		workersMatchTopology, err := checkWorkersMatchTopology(clusterName, namespace, workerGroupSpec)
		if err != nil {
			return nil, err
		}

		if !workersMatchTopology {
			admit = false
			status = "Failure"
			message = "Number of workers in worker group not equal to specified topology"
			break
		}

		// If sub-slicing is requested, ensure we can find a satisfying topology key.
		if desiredSubslice, subsliceRequested := workerGroupSpec.Template.Annotations[tpuSubsliceTopologyAnnotation]; subsliceRequested {
			warning, admitErr, err := t.checkSubsliceAffinity(workerGroupSpec, desiredSubslice)
			if err != nil {
				return nil, err
			}
			if admitErr != nil {
				admit = false
				status = "Failure"
				message = admitErr.Error()
				break
			}
			if len(warning) > 0 {
				warnings = append(warnings, warning)
			}
		}
	}

	// Create AdmissionResponse
	admissionResponse := &admissionv1.AdmissionResponse{
		UID:     admissionReview.Request.UID,
		Allowed: admit,
		Result: &metav1.Status{
			Status:  status,
			Message: message,
		},
		Warnings: warnings,
	}
	return admissionResponse, nil
}

func (t *TPUWebhookServer) checkSubsliceAffinity(workerGroupSpec ray.WorkerGroupSpec, desiredSubslice string) (warning string, admitErr, err error) {
	numHosts := int(workerGroupSpec.NumOfHosts)
	if numHosts <= 1 {
		// Single-host workers can run anywhere.
		return "", nil, nil
	}

	parentTopology := workerGroupSpec.Template.Spec.NodeSelector[tpuTopologyLabel]
	if parentTopology == "" {
		return "", fmt.Errorf("must specify parent topology %q in nodeSelector when using subslice annotation", tpuTopologyLabel), nil
	}

	if !sliceIsSubset(parentTopology, desiredSubslice) {
		return "", fmt.Errorf("subslice %q is not smaller than parent %q", desiredSubslice, parentTopology), nil
	}

	selector := labels.SelectorFromSet(workerGroupSpec.Template.Spec.NodeSelector)
	nodes, err := t.nodeLister.List(selector)
	if err != nil {
		return "", nil, err
	}

	if len(nodes) == 0 {
		warning := fmt.Sprintf("Subsliced TPU worker group %q targets zero nodes (cannot discover subslice affinity) and will need to be re-created after nodes are provisioned.", workerGroupSpec.GroupName)
		return warning, nil, nil
	}

	tpuType := workerGroupSpec.Template.Spec.NodeSelector[gkeTPUAcceleratorLabel]
	topology := buildTPUTopology(nodes)
	if topology.missingTopologyInfo() {
		return "", fmt.Errorf("cannot subslice TPU type %q without Dynamic Slicing (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/dynamic-slicing)", tpuType), nil
	}
	topology.prettyPrint()
	topologyKey, err := topology.subsliceAffinityKey(numHosts)
	if err != nil {
		return "", fmt.Errorf("ambiguous subslice: %w", err), nil
	}

	// The mapping of TPU type, slice, and subslice should always
	// result in the same topologyKey. Log this mapping for
	// debuggability.
	klog.V(0).Infof("subslice (type=%q, parent=%q, slice=%q) -> %q", tpuType, parentTopology, desiredSubslice, topologyKey)

	return "", nil, nil
}

// sliceIsSubset returns true if the parent slice is dimensionally at least as
// big as the child, i.e. the child square or cube fits within the parent.
func sliceIsSubset(parent, child string) bool {
	parentDims := strings.Split(parent, "x")
	childDims := strings.Split(child, "x")
	if len(parentDims) != len(childDims) {
		return false
	}
	for i := range len(parentDims) {
		parentVal, err1 := strconv.Atoi(parentDims[i])
		childVal, err2 := strconv.Atoi(childDims[i])
		if err1 != nil || err2 != nil {
			return false
		}
		if childVal > parentVal {
			return false
		}
	}
	return true
}

// getEnvironmentVariable returns value associated with a given Container environment variable and if it exists
func getEnvironmentVariable(varName string, container corev1.Container) (string, bool) {
	if container.Env != nil && len(container.Env) > 0 {
		for _, envVar := range container.Env {
			if envVar.Name == varName {
				if envVar.Value != "" {
					return envVar.Value, true
				} else if envVar.ValueFrom != nil {
					return "", true
				}
			}
		}
	}
	return "", false
}

// getReplicaIndex returns the next lowest-index Pod Slice (worker group replica) to assign a Pod to in the RayCluster
// there are three possible cases here:
//  1. sliceToTPUHosts is empty, this is the first pod the webhook intercepts
//     - assign this pod to replica 0
//  2. The Pod Slice exists in sliceToTPUHosts, but has # created workers < NumOfHosts
//     - assign this pod to the lowest index replica with # created workers < NumOfHosts
//     pods to the same replica
//  3. sliceToTPUHosts isn't empty, but all slices have # workers == NumOfHosts
//     - this occurs when the pod we intercept is the first pod of a different slice in the cluster
//     - we keep track of how many replicas of the same worker group have been added to sliceToTPUHosts
//     so far, and assign this pod to the next integer replicaIndex
func getReplicaIndex(sliceToTPUHosts map[slice][]int, clusterName string, groupName string, namespace string) int {
	// first pod created in cluster
	if len(sliceToTPUHosts) == 0 {
		return 0
	}
	nextLowestId := math.MaxInt32
	existingIndices := make(map[int]bool, len(sliceToTPUHosts))
	for slice, workerList := range sliceToTPUHosts {
		if slice.clusterName == clusterName && slice.groupName == groupName && slice.namespace == namespace {
			existingIndices[slice.replicaIndex] = true
			createdPods := len(workerList)
			if createdPods < int(slice.numOfHosts) {
				if slice.replicaIndex < nextLowestId {
					nextLowestId = slice.replicaIndex
				}
			}
		}
	}
	// If no ID was found, this is the first pod of a new slice in the cluster,
	// which should either be added to the end (one plus the last observed
	// slice) or slot into an existing gap (e.g. a previous slice was
	// preempted).
	if nextLowestId == math.MaxInt32 {
		// By the pigeonhole principle, there must be at least one missing
		// replica index in the range [0, len(existingIndices)].
		for i := range len(existingIndices) + 1 {
			if !existingIndices[i] {
				nextLowestId = i
				break
			}
		}
	}
	klog.V(1).InfoS("getReplicaIndex", "RayCluster", namespace+"/"+clusterName, "Worker Group", groupName, "Replica Index", nextLowestId)
	return nextLowestId
}

// getNextWorkerID returns the next lowest TPU_WORKER_ID in the Pod Slice
func getNextWorkerID(sliceToTPUHosts map[slice][]int, podSlice slice, namespace string, replicaIndex int) (int, error) {
	tpuWorkerID := 0 // defaults to 0 (first Pod in slice)
	if len(sliceToTPUHosts) == 0 || len(sliceToTPUHosts[podSlice]) == 0 {
		return tpuWorkerID, nil
	}
	sort.Ints(sliceToTPUHosts[podSlice])
	// iterate through existing workers and get the next lowest, unused ID
	lastID := 0
	for index, workerID := range sliceToTPUHosts[podSlice] {
		// check for incorrect assignment of IDs
		if index == 0 {
			lastID = workerID
		} else if workerID == lastID {
			return 0, errors.New("Identical TPU_WORKER_ID assigned to multiple TPU workers in slice")
		}
		// get the next lowest, valid TPU_WORKER_ID
		if workerID != tpuWorkerID {
			break
		}
		lastID = workerID
		tpuWorkerID++
	}
	klog.V(1).InfoS("getNextWorkerID", "RayCluster", namespace+"/"+podSlice.clusterName, "Worker Group", podSlice.groupName, legacyReplicaIndexLabelKey, replicaIndex, "TPU_WORKER_ID", tpuWorkerID)
	return tpuWorkerID, nil
}

// getSliceToTPUHosts returns a mapping representing the current RayCluster state of TPU pods using a PodLister
func (t *TPUWebhookServer) getSliceToTPUHosts(clusterName string, groupName string, namespace string, numOfHosts int32) (map[slice][]int, error) {
	sliceToTPUHosts := make(map[slice][]int)

	// we only care about workers in the same RayCluster and worker group when assigning IDs
	podsInGroup, err := t.podLister.Pods(namespace).List(labels.SelectorFromSet(labels.Set{utils.RayClusterLabelKey: clusterName, utils.RayNodeGroupLabelKey: groupName}))
	if err != nil {
		return nil, err
	}

	if podsInGroup == nil {
		// return an empty mapping if no Pods with 'ray.io/group' label found
		return sliceToTPUHosts, nil
	}
	klog.V(1).InfoS("getSliceToTPUHosts", "RayCluster", namespace+"/"+clusterName, "# Pods in Group", len(podsInGroup))
	for _, existingPod := range podsInGroup {
		if existingPod.DeletionTimestamp != nil {
			continue
		}
		existingNamespace := existingPod.Namespace
		// check that Pods are in the same namespace
		if namespace != existingNamespace {
			continue
		}

		if !containerRequestingTPUs(existingPod.Spec.Containers...) {
			// Pod does not request TPUs, 'ray.io/group' is not a TPU worker group
			return sliceToTPUHosts, nil
		}
		replicaIndexLabel := existingPod.Labels[legacyReplicaIndexLabelKey]
		if replicaIndexLabel == "" {
			// Pod has not been intercepted by the KubeRay TPU webhook yet
			continue
		}
		replicaIndexLabelValues := strings.Split(replicaIndexLabel, "-")
		existingReplicaIndex, _ := strconv.Atoi(replicaIndexLabelValues[len(replicaIndexLabelValues)-1])

		// Number of containers in this Pod to index.
		numTpuContainers := 0
		for _, c := range existingPod.Spec.Containers {
			if containerRequestingTPUs(c) {
				numTpuContainers++
			}
		}

		podSlice := slice{clusterName, groupName, namespace, existingReplicaIndex, numOfHosts}
		hostIndex := -1
		for _, container := range existingPod.Spec.Containers {
			if !containerRequestingTPUs(container) {
				continue
			}

			tpuWorkerIDEnvVar, _ := getEnvironmentVariable("TPU_WORKER_ID", container)
			if tpuWorkerIDEnvVar == "" {
				// If the container has been admitted without a TPU_WORKER_ID, return
				// an error as this will cause the TPU slice to fail JAX initialization.
				return nil, errors.New("existing TPU worker missing TPU_WORKER_ID")
			}
			foundWorkerID, err := strconv.Atoi(tpuWorkerIDEnvVar)
			if err != nil {
				klog.ErrorS(err, "getSliceToTPUHosts", "RayCluster", namespace+"/"+clusterName, "TPU_WORKER_ID", tpuWorkerIDEnvVar)
				continue
			}
			// Determine the host this worker is running on.
			hostIndex = foundWorkerID / numTpuContainers
			break
		}
		if hostIndex != -1 {
			if sliceToTPUHosts[podSlice] == nil {
				sliceToTPUHosts[podSlice] = []int{hostIndex}
			} else {
				sliceToTPUHosts[podSlice] = append(sliceToTPUHosts[podSlice], hostIndex)
			}
			klog.V(1).InfoS("getSliceToTPUHosts", "RayCluster", namespace+"/"+clusterName, legacyReplicaIndexLabelKey, existingReplicaIndex, "HostIndex", hostIndex)
		}
	}
	return sliceToTPUHosts, nil
}

// extractPod returns a Pod unmarshalled from an Admission Request
func extractPod(admissionReview *admissionv1.AdmissionReview) (*corev1.Pod, error) {
	if admissionReview.Request.Kind.Kind != "Pod" {
		return nil, fmt.Errorf("Expected Pod but got %s", admissionReview.Request.Kind.Kind)
	}

	pod := corev1.Pod{}
	if admissionReview.Request.Operation == "CREATE" {
		if err := json.Unmarshal(admissionReview.Request.Object.Raw, &pod); err != nil {
			return nil, err
		}
	}

	return &pod, nil
}

// addEnvVarPatch is a helper to create a JSON patch to add an environment variable to a container.
// It returns the updated patch slice and a boolean indicating that the env array now exists.
func addEnvVarPatch(patches []patch, envVar corev1.EnvVar, path string, envArrayExists bool) ([]patch, bool) {
	p := patch{"op": "add"}
	if envArrayExists {
		// Env array exists, append to it.
		p["path"] = fmt.Sprintf("%s/-", path)
		p["value"] = envVar
	} else {
		// Env array doesn't exist, create it.
		p["path"] = path
		p["value"] = []corev1.EnvVar{envVar}
	}
	patches = append(patches, p)

	return patches, true
}

// mutatePod returns an Admission Response after injecting TPU related fields to a given Pod
func (t *TPUWebhookServer) mutatePod(admissionReview *admissionv1.AdmissionReview) (*admissionv1.AdmissionResponse, error) {
	pod, err := extractPod(admissionReview)
	if err != nil {
		return nil, err
	}

	var patches []patch
	admissionResponse := &admissionv1.AdmissionResponse{
		UID:     admissionReview.Request.UID,
		Allowed: true,
	}

	containers := pod.Spec.Containers
	if containers == nil {
		return nil, errors.New("Container path not specified")
	}
	if !containerRequestingTPUs(containers...) {
		// if no TPUs are requested, simply admit the Pod
		return admissionResponse, nil
	}

	// ray operator only sets GenerateName field - doesn't include random suffix until after admission request
	// use mapping of {cluster name, group name, replicaIndex} -> workers to extract next TPU_WORKER_ID
	clusterName := pod.Labels[utils.RayClusterLabelKey]
	if clusterName == "" {
		return nil, errors.New("Ray Pod created by KubeRay missing RayCluster label")
	}
	groupName := pod.Labels[utils.RayNodeGroupLabelKey]
	if groupName == "" {
		return nil, errors.New("Ray Pod created by KubeRay missing Group label")
	}
	namespace := pod.Namespace
	topology, ok := pod.Annotations[tpuSubsliceTopologyAnnotation]
	if !ok {
		topology = pod.Spec.NodeSelector[tpuTopologyLabel]
	}
	if topology == "" {
		return nil, errors.New("Ray Pod created by KubeRay missing TPU topology nodeSelector")
	}
	// assign worker to the next unique ID in the Pod Slice and update map
	chipsPerHost := getNumTPUChipsRequested(containers...)
	numOfHosts, _ := getNumTPUHostsFromTopology(clusterName, groupName, namespace, topology, chipsPerHost) // ignore error here because topology may not be set yet

	// Detect v7x TPU accelerator
	isV7x := false
	if selector, ok := pod.Spec.NodeSelector[gkeTPUAcceleratorLabel]; ok && strings.HasPrefix(selector, tpu7xType) {
		isV7x = true
	}

	// v7x (Ironwood) TPUs can run multiple NUMA-aligned containers per Pod to optimize
	// memory bandwidth across the dual-chiplet architecture. We count them to assign
	// unique worker IDs and network ports to each independent ML process.
	numTpuContainers := 0
	for _, c := range containers {
		if containerRequestingTPUs(c) {
			numTpuContainers++
		}
	}

	// Pod indexing variables from K8s labels or assigned dynamically.
	var replicaIndex int
	var tpuWorkerID int

	replicaIndexStr, hasReplicaLabel := pod.Labels[utils.RayWorkerReplicaIndexKey]
	hostIndexStr, hasHostLabel := pod.Labels[utils.RayHostIndexKey]

	if hasReplicaLabel && hasHostLabel {
		// In KubeRay v1.5, Pod indexing is performed by RayCluster controller when RayMultiHostIndexing
		// feature is enabled.
		klog.V(1).InfoS("mutatePod", "Found Ray indexing labels for KubeRay Pod", pod.Name)

		var err error
		replicaIndex, err = strconv.Atoi(replicaIndexStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse replica index label: %w", err)
		}

		tpuWorkerID, err = strconv.Atoi(hostIndexStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse host index label: %w", err)
		}
	} else {
		// Fallback for older KubeRay versions that do not set K8s index labels.
		replicaIndex, tpuWorkerID, err = t.legacyAssignIndices(pod, clusterName, groupName, namespace, numOfHosts, &patches)
		if err != nil {
			return nil, err
		}
	}

	headlessServiceName := generateHeadlessServiceName(clusterName)

	if numOfHosts > 1 {
		// inject hostname and subdomain into pod spec for DNS records
		hostname := fmt.Sprintf("%s-%d-%d", groupName, replicaIndex, tpuWorkerID)
		klog.V(1).InfoS("mutatePod", "RayCluster", namespace+"/"+clusterName, "hostname", hostname)
		hostnamePatch := patch{"op": "add", "path": "/spec/hostname", "value": hostname}
		patches = append(patches, hostnamePatch)

		injectSubdomain(clusterName, &patches)

		// inject pod affinity/anti-affinity for scheduling.
		if err := t.injectAffinity(pod, replicaIndex, int(numOfHosts), groupName, &patches); err != nil {
			return nil, err
		}
	}

	// inject all environment variables into the container requesting TPUs
	tpuContainerIndex := 0
	for i := 0; i < len(containers); i++ {
		container := containers[i]
		if containerRequestingTPUs(container) {
			path := fmt.Sprintf("/spec/containers/%d/env", i)
			isEnvInitialized := len(container.Env) > 0

			// For TPU generations up until v6e, TPU_WORKER_ID was indexed per TPU Pod.
			// With Ironwood TPU and newer, worker IDs are unique per container requesting TPU.
			finalWorkerID := tpuWorkerID
			if isV7x {
				finalWorkerID = (tpuWorkerID * numTpuContainers) + tpuContainerIndex
			}

			// Multi-Slice variable injection logic.
			if _, exists := getEnvironmentVariable("MEGASCALE_NUM_SLICES", container); exists {
				// Set MEGASCALE_SLICE_ID, the index of this replica within the worker group.
				// Multiple TPU multi-host replicas in the same worker group are assumed to be apart
				// of a multi-slice configuration.
				if val, _ := getEnvironmentVariable("MEGASCALE_SLICE_ID", container); val == "" {
					patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{
						Name:  "MEGASCALE_SLICE_ID",
						Value: fmt.Sprint(replicaIndex),
					}, path, isEnvInitialized)
				}

				// Set MEGASCALE_COORDINATOR_ADDRESS, the address of worker 0 of slice 0.
				if val, _ := getEnvironmentVariable("MEGASCALE_COORDINATOR_ADDRESS", container); val == "" {
					coordAddress := fmt.Sprintf("%s-0-0.%s", groupName, headlessServiceName)
					if isV7x {
						// Target container 0's port
						coordAddress = fmt.Sprintf("%s:%d", coordAddress, megascalePortBase)
					}
					patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{
						Name:  "MEGASCALE_COORDINATOR_ADDRESS",
						Value: coordAddress,
					}, path, isEnvInitialized)
				}

				// Set MEGASCALE_PORT, defaulting to 8081 since 8080 is used by Ray for metrics.
				megascalePort := fmt.Sprint(megascalePortBase)
				if isV7x {
					megascalePort = fmt.Sprint(megascalePortBase + tpuContainerIndex)
				}
				if val, _ := getEnvironmentVariable("MEGASCALE_PORT", container); val == "" {
					patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{
						Name:  "MEGASCALE_PORT",
						Value: megascalePort,
					}, path, isEnvInitialized)
				}
			}
			// Network addressing injection logic.
			if numOfHosts > 1 {
				// Legacy: inject TPU_WORKER_HOSTNAMES
				val, _ := getEnvironmentVariable("TPU_WORKER_HOSTNAMES", container)
				if val == "" || val == "localhost" {
					// Inject value if unset or set to default value.
					hostnames, err := genDNSHostnames(numOfHosts, groupName, clusterName, namespace, replicaIndex)
					if err != nil {
						return nil, err
					}
					klog.V(1).InfoS("mutatePod", "RayCluster", namespace+"/"+clusterName, "TPU_WORKER_HOSTNAMES", hostnames)
					isEnvInitialized = injectHostnames(clusterName, hostnames, path, container, &patches, isEnvInitialized)
				}

				if isV7x {
					// v7x only: Inject TPU_PROCESS_ADDRESSES & TPU_PROCESS_PORT
					val, _ := getEnvironmentVariable("TPU_PROCESS_ADDRESSES", container)
					if val == "" || strings.Contains(val, "localhost") {
						// Inject value if unset or set to default value.
						processAddresses, err := getTPUProcessAddresses(numOfHosts, numTpuContainers, groupName, clusterName, replicaIndex)
						if err != nil {
							return nil, err
						}
						klog.V(1).InfoS("mutatePod v7x", "RayCluster", namespace+"/"+clusterName, "TPU_PROCESS_ADDRESSES", processAddresses)
						patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{Name: "TPU_PROCESS_ADDRESSES", Value: processAddresses}, path, isEnvInitialized)
					}

					if val, _ := getEnvironmentVariable("TPU_PROCESS_PORT", container); val == "" {
						patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{Name: "TPU_PROCESS_PORT", Value: fmt.Sprint(tpuProcessPortBase + tpuContainerIndex)}, path, isEnvInitialized)
					}
				}
			}
			// inject TPU_WORKER_ID
			valWorkerID, _ := getEnvironmentVariable("TPU_WORKER_ID", container)
			expectedID := fmt.Sprint(finalWorkerID)
			isDefault := valWorkerID == fmt.Sprint(tpuContainerIndex)

			if valWorkerID == "" || (isDefault && valWorkerID != expectedID) {
				// Overwrite ID if set incorrectly by user, or set to default erroneously in a multi-host env.
				klog.V(1).InfoS("mutatePod", "RayCluster", namespace+"/"+clusterName, "TPU_WORKER_ID", finalWorkerID, "Replica Index", replicaIndex)
				workerID := corev1.EnvVar{
					Name:  "TPU_WORKER_ID",
					Value: fmt.Sprint(finalWorkerID),
				}
				patches, isEnvInitialized = addEnvVarPatch(patches, workerID, path, isEnvInitialized)
			}
			// inject TPU_NAME
			if value, _ := getEnvironmentVariable("TPU_NAME", container); value == "" {
				tpuNameValue := fmt.Sprintf("%s-%d", groupName, replicaIndex)
				klog.V(1).InfoS("mutatePod", "RayCluster", namespace+"/"+clusterName, "TPU_NAME", tpuNameValue, "Replica Index", replicaIndex)
				patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{
					Name:  "TPU_NAME",
					Value: tpuNameValue,
				}, path, isEnvInitialized)
			}

			// inject TPU_DEVICE_PLUGIN_HOST_IP. This Env Var is required for TPU_DEVICE_PLUGIN_ADDR
			if _, exists := getEnvironmentVariable("TPU_DEVICE_PLUGIN_HOST_IP", container); !exists {
				klog.V(1).InfoS("mutatePod, add TPU_DEVICE_PLUGIN_HOST_IP", "RayCluster", namespace+"/"+clusterName)
				tpuDevicePluginAddr := corev1.EnvVar{
					Name: "TPU_DEVICE_PLUGIN_HOST_IP",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "status.hostIP",
						},
					},
				}
				patches, isEnvInitialized = addEnvVarPatch(patches, tpuDevicePluginAddr, path, isEnvInitialized)
			}

			// inject TPU_DEVICE_PLUGIN_ADDR
			if value, _ := getEnvironmentVariable("TPU_DEVICE_PLUGIN_ADDR", container); value == "" {
				tpuDevicePluginAddrValue := "$(TPU_DEVICE_PLUGIN_HOST_IP):2112"
				klog.V(1).InfoS("mutatePod", "RayCluster", namespace+"/"+clusterName, "TPU_DEVICE_PLUGIN_ADDR", tpuDevicePluginAddrValue)
				patches, isEnvInitialized = addEnvVarPatch(patches, corev1.EnvVar{
					Name:  "TPU_DEVICE_PLUGIN_ADDR",
					Value: tpuDevicePluginAddrValue,
				}, path, isEnvInitialized)
			}
			tpuContainerIndex++
		}
	}

	patchBytes, err := json.Marshal(patches)
	if err != nil {
		return nil, err
	}

	admissionResponse.Patch = patchBytes
	admissionResponse.PatchType = func() *admissionv1.PatchType {
		pt := admissionv1.PatchTypeJSONPatch
		return &pt
	}()
	return admissionResponse, nil
}

// legacyAssignIndices assigns replicaIndex and tpuWorkerID for older KubeRay versions.
func (t *TPUWebhookServer) legacyAssignIndices(pod *corev1.Pod, clusterName, groupName, namespace string, numOfHosts int32, patches *[]patch) (int, int, error) {
	t.cacheMutex.Lock()
	defer t.cacheMutex.Unlock()

	// Wait for PodInformer cache to update from previous requests.
	timedOut := t.cacheCond.Wait(1 * time.Second)
	if timedOut {
		klog.V(0).Infof("Mutating pod %s with a stale cache", pod.GetName())
	}

	// Query k8s client to populate sliceToTPUHosts.
	sliceToTPUHosts, err := t.getSliceToTPUHosts(clusterName, groupName, namespace, numOfHosts)
	if err != nil {
		return 0, 0, err
	}

	replicaIndex := getReplicaIndex(sliceToTPUHosts, clusterName, groupName, namespace)
	podSlice := slice{clusterName, groupName, namespace, replicaIndex, numOfHosts}
	tpuWorkerID, err := getNextWorkerID(sliceToTPUHosts, podSlice, namespace, replicaIndex)
	if err != nil {
		return 0, 0, err
	}

	// Update state for next request.
	t.cacheCond.Admit(fmt.Sprintf("%s-%s-%s-%d-%d", namespace, clusterName, groupName, replicaIndex, tpuWorkerID))

	// Manually inject the replicaIndex label
	injectReplicaLabel(clusterName, namespace, replicaIndex, groupName, patches)

	return replicaIndex, tpuWorkerID, nil
}

// buildTLSConfig builds a TLS config for the webhook server.
// It supports two modes:
//  1. Using cert-watcher with a context and file paths specified by CertFile and KeyFile flags.
//  2. Using static certificates, either from the ServerCert and ServerKey base64-encoded flags,
//     or by reading directly from the default certPath and keyPath.
func buildTLSConfig(ctx context.Context) (*tls.Config, error) {
	if CertFile != "" && KeyFile != "" {
		klog.Infof("Using cert-watcher for TLS. CertFile: %s, KeyFile: %s", CertFile, KeyFile)
		cw, err := certwatcher.New(CertFile, KeyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to initialize cert watcher: %w", err)
		}

		go func() {
			klog.V(1).Info("Starting certificate watcher...")
			if err := cw.Start(ctx); err != nil {
				klog.Errorf("certificate watcher error: %v", err)
			}
		}()

		return &tls.Config{
			GetCertificate: cw.GetCertificate,
		}, nil
	}

	var certPEM, keyPEM []byte
	var err error

	if ServerCert != "" && ServerKey != "" {
		klog.Infof("Using base64-encoded flags for TLS.")
		certPEM, err = base64.StdEncoding.DecodeString(ServerCert)
		if err != nil {
			return nil, fmt.Errorf("failed to decode server cert: %w", err)
		}
		keyPEM, err = base64.StdEncoding.DecodeString(ServerKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode server key: %w", err)
		}
	} else {
		klog.Infof("Using certificate files for TLS. CertPath: %s, KeyPath: %s", certPath, keyPath)
		certPEM, err = os.ReadFile(certPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read server cert file %s: %w", certPath, err)
		}
		keyPEM, err = os.ReadFile(keyPath)
		if err != nil {
			return nil, fmt.Errorf("failed to read server key file %s: %w", keyPath, err)
		}
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("failed to create x509 key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
	}, nil
}

func init() {
	flag.StringVar(&BindAddr, "bind-address", ":443", "Address to bind HTTPS service to")
	flag.StringVar(&KubeConfigPath, "kube-config-path", "", "Kubernetes config path for k8s client")
	flag.StringVar(&ServerCert, "server-cert", "", "base64-encoded server certificate for TLS")
	flag.StringVar(&ServerKey, "server-key", "", "base64-encoded server key for TLS")
	flag.StringVar(&CertFile, "tls-cert-file", "", "File containing the x509 Certificate for HTTPS")
	flag.StringVar(&KeyFile, "tls-private-key-file", "", "File containing the x509 private key matching --tls-cert-file.")

	// set klog verbosity level
	klog.InitFlags(nil)
}

// addPod allows next goroutine to start once the webhook PodInformer cache updates
func (t *TPUWebhookServer) addPod(obj interface{}) {
	pod := obj.(*corev1.Pod)
	klog.V(1).InfoS("addPod", "Pod", pod.Namespace+"/"+pod.Name, "Time", time.Now())
	if _, err := t.isLastAdmittedPod(pod); err != nil {
		klog.V(0).ErrorS(err, "Failed to verify local cache from pod informer update", "name", pod.GetName())
	}
}

func (t *TPUWebhookServer) isLastAdmittedPod(pod *corev1.Pod) (bool, error) {
	if pod.Spec.Containers == nil || !containerRequestingTPUs(pod.Spec.Containers...) {
		// Pod does not use TPUs.
		return false, nil
	}
	replicaIndex := pod.Labels[legacyReplicaIndexLabelKey]
	if replicaIndex == "" {
		// Pod was not mutated by the webhook.
		return false, nil
	}
	clusterName := pod.Labels[utils.RayClusterLabelKey]
	if clusterName == "" {
		return false, errors.New("Ray Pod created by KubeRay missing RayCluster label")
	}
	namespace := pod.Namespace
	for _, container := range pod.Spec.Containers {
		if !containerRequestingTPUs(container) {
			// Skip to the next container.
			continue
		}
		tpuWorkerID, _ := getEnvironmentVariable("TPU_WORKER_ID", container)
		if tpuWorkerID == "" {
			// TPU pod container was not intercepted by the webhook.
			continue
		}

		// The pod has a TPU-configured container. Inform the cache control
		// mechanism of the pod id. It's sufficient return after inspecting the
		// first TPU container (there can be no second TPU container with a
		// different id).
		uniquePodID := fmt.Sprintf("%s-%s-%s-%s", namespace, clusterName, replicaIndex, tpuWorkerID)
		return t.cacheCond.Signal(uniquePodID), nil
	}
	return false, nil
}

// podSyncCond provides a synchronization primitive that allows the mutating
// webhook to unblock one in-flight request at a time when the informer cache is
// ready.
type podSyncCond struct {
	m            sync.Mutex
	wakeChan     chan struct{}
	synced       bool
	lastAdmitted string
}

func newPodSyncCond() *podSyncCond {
	c := &podSyncCond{
		wakeChan: make(chan struct{}, 1),
		synced:   true,
	}
	return c
}

// Wait blocks until the cache is up-to-date or the timeout is reached.
// up-to-date.
func (c *podSyncCond) Wait(timeout time.Duration) (timedout bool) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// The lock must be held while reading or writing c.synced, but cannot be
	// held while blocking on the channel or timer.
	c.m.Lock()
	defer c.m.Unlock()

	for !c.synced { // Lock is held while reading state.
		c.m.Unlock() // Release lock while waiting to be unblocked.
		select {
		case <-c.wakeChan:
			c.m.Lock()
			continue // Try checking t.synced again with lock held.
		case <-timer.C:
			// If cache does not sync within the timeout, assume the
			// lastAdmitted pod will never appear. Release the cache sync bit
			// and return.
			c.m.Lock()
			klog.V(0).Infof("Timed out waiting for pod %q to be added to cache", c.lastAdmitted)
			c.synced = true
			return true
		}
	}

	// Drain wakeChan of any pre-buffered events that were waiting before Wait
	// was called.
drain:
	for {
		select {
		case <-c.wakeChan:
		default:
			break drain
		}
	}
	return false
}

// Admit invalidates the cache and blocks Lock until the admitted pod has synced
// to the cache.
func (c *podSyncCond) Admit(pod string) {
	c.m.Lock()
	defer c.m.Unlock()
	c.synced = false
	c.lastAdmitted = pod
}

// Signal marks a pod as received in the cache. The lock does not need to be
// held. Returns true if the cache is marked synced as a result.
func (c *podSyncCond) Signal(pod string) bool {
	c.m.Lock()
	defer c.m.Unlock()
	// If the pod received by the informer matches the one we last admitted --
	// and were waiting for -- then we can mark the cache as synced and wake one
	// in-flight request.
	if pod == c.lastAdmitted {
		c.synced = true
		c.lastAdmitted = ""

		// Wake by sending to wakeChan. Select with an empty default allows us
		// to skip if there is no blocked request.
		select {
		case c.wakeChan <- struct{}{}:
		default:
		}

		return true
	}
	return false
}

// TPUTopology indexes the statically discoverable topology labels for TPU
// nodes.
type TPUTopology struct {
	Hosts     map[string][]*corev1.Node
	Subblocks map[string][]*corev1.Node
	Blocks    map[string][]*corev1.Node
	NodePools map[string][]*corev1.Node
}

func buildTPUTopology(nodes []*corev1.Node) *TPUTopology {
	t := &TPUTopology{
		Hosts:     make(map[string][]*corev1.Node),
		Subblocks: make(map[string][]*corev1.Node),
		Blocks:    make(map[string][]*corev1.Node),
		NodePools: make(map[string][]*corev1.Node),
	}
	for _, node := range nodes {
		var (
			pool     = node.Labels[gkeNodePoolLabel]
			block    = node.Labels[gceTopologyBlockLabel]
			subblock = node.Labels[gceTopologySubblockLabel]
			host     = node.Labels[gceTopologyHostLabel]
		)

		if pool != "" {
			t.NodePools[pool] = append(t.NodePools[pool], node)
		}
		if block != "" {
			t.Blocks[block] = append(t.Blocks[block], node)
		}
		if subblock != "" {
			t.Subblocks[subblock] = append(t.Subblocks[subblock], node)
		}
		if host != "" {
			t.Hosts[host] = append(t.Hosts[host], node)
		}
	}
	return t
}

// missingTopologyInfo returns true if the topology struct has nodes in a
// nodepool, but none can be indexed by GCE topology label.
func (t *TPUTopology) missingTopologyInfo() bool {
	return len(t.NodePools) > 0 && len(t.Hosts) == 0 && len(t.Subblocks) == 0 && len(t.Blocks) == 0
}

// prettyPrint logs the sizes of the node label select-able TPU groups.
func (t *TPUTopology) prettyPrint() {
	klog.V(0).Info("TPU Topology:")
	klog.V(0).Infof("  Host      has %d groups of sizes %v", len(t.Hosts), sliceLengths(t.Hosts))
	klog.V(0).Infof("  Subblocks has %d groups of sizes %v", len(t.Subblocks), sliceLengths(t.Subblocks))
	klog.V(0).Infof("  Blocks    has %d groups of sizes %v", len(t.Blocks), sliceLengths(t.Blocks))
	klog.V(0).Infof("  NodePools has %d groups of sizes %v", len(t.NodePools), sliceLengths(t.NodePools))
}

func sliceLengths[K comparable, T any](m map[K][]T) []int {
	lens := make(map[int]struct{})
	for _, s := range m {
		lens[len(s)] = struct{}{}
	}
	return slices.Collect(maps.Keys(lens))
}

// subsliceAffinityKey looks up which node label (if any) selects a set of nodes
// of size numHosts.
func (t *TPUTopology) subsliceAffinityKey(numHosts int) (string, error) {
	for name, nodes := range t.Hosts {
		if len(nodes) == numHosts {
			klog.V(0).Infof("%d workers can use TPU host %q", numHosts, name)
			return gceTopologyHostLabel, nil
		}
	}
	for name, nodes := range t.Subblocks {
		if len(nodes) == numHosts {
			klog.V(0).Infof("%d workers can use TPU subblock %q", numHosts, name)
			return gceTopologySubblockLabel, nil
		}
	}
	for name, nodes := range t.Blocks {
		if len(nodes) == numHosts {
			klog.V(0).Infof("%d workers can use TPU block %q", numHosts, name)
			return gceTopologyBlockLabel, nil
		}
	}
	for name, nodes := range t.NodePools {
		if len(nodes) == numHosts {
			klog.V(0).Infof("%d workers can use entire nodepool %q", numHosts, name)
			return gkeNodePoolLabel, nil
		}
	}
	return "", fmt.Errorf("could not find affinity rule to schedule %d hosts", numHosts)
}

// startServer sets up and runs the webhook's HTTP server.
func startServer(tpuWebhookServer *TPUWebhookServer) error {
	klog.V(0).Info("Starting KubeRay TPU webhook server...")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "kuberay-tpu-webhook")
	})
	mux.HandleFunc("/mutate", tpuWebhookServer.Mutate)
	mux.HandleFunc("/validate", tpuWebhookServer.Validate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tlsConfig, err := buildTLSConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to build TLS config: %w", err)
	}

	srv := &http.Server{
		Addr:      BindAddr,
		Handler:   mux,
		TLSConfig: tlsConfig,
	}

	klog.Infof("HTTPS server listening on %s", BindAddr)
	return srv.ListenAndServeTLS("", "")
}

func main() {
	flag.Parse()

	// Validate that only one set of cert flags are  used together.
	serverCertFlagsSet := ServerCert != "" || ServerKey != ""
	certFileFlagsSet := CertFile != "" || KeyFile != ""
	if serverCertFlagsSet && certFileFlagsSet {
		klog.Fatal("Configuration error: both TLS base64 and path flags defined at the same time.")
	}

	// use in-cluster config if kubeConfig path is not passed as a flag
	var client *kubernetes.Clientset
	if KubeConfigPath == "" {
		config, err := rest.InClusterConfig()
		if err != nil {
			panic(err)
		}
		client = kubernetes.NewForConfigOrDie(config)
	} else {
		config, err := clientcmd.BuildConfigFromFlags("", KubeConfigPath)
		if err != nil {
			panic(err)
		}
		client = kubernetes.NewForConfigOrDie(config)
	}

	// instantiate PodInformer for Ray worker pods in the GKE cluster
	tweakListOptionsFunc := func(options *metav1.ListOptions) {
		options.LabelSelector = "ray.io/node-type=worker,app.kubernetes.io/created-by=kuberay-operator"
	}
	factory := informers.NewFilteredSharedInformerFactory(client, 1*time.Minute, metav1.NamespaceAll, tweakListOptionsFunc)
	podInformer := factory.Core().V1().Pods().Informer()

	// instantiate NodeInformer for TPU nodes in the GKE cluster
	tweakNodeListOptionsFunc := func(options *metav1.ListOptions) {
		options.LabelSelector = gkeTPUAcceleratorLabel
	}
	nodeFactory := informers.NewFilteredSharedInformerFactory(client, 1*time.Minute, metav1.NamespaceAll, tweakNodeListOptionsFunc)
	nodeInformer := nodeFactory.Core().V1().Nodes().Informer()

	// start the Informers and wait for cache sync
	stopCh := make(chan struct{})
	factory.Start(stopCh)
	nodeFactory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)
	nodeFactory.WaitForCacheSync(stopCh)

	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
		klog.Fatal("Timed out waiting for PodInformer to sync")
	}
	if !cache.WaitForCacheSync(stopCh, nodeInformer.HasSynced) {
		klog.Fatal("Timed out waiting for NodeInformer to sync")
	}

	podLister := factory.Core().V1().Pods().Lister()
	nodeLister := nodeFactory.Core().V1().Nodes().Lister()

	if podLister == nil {
		klog.Fatal("Failed to initialize Pod Lister")
	}
	if nodeLister == nil {
		klog.Fatal("Failed to initialize Node Lister")
	}

	// close the informers on exit
	defer close(stopCh)

	tpuWebhookServer := NewTPUWebhookServer(podLister, nodeLister)

	// Add custom event handler for Pod creation
	podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: tpuWebhookServer.addPod,
		},
	)

	// Start the webhook server.
	if err := startServer(tpuWebhookServer); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.Fatalf("Failed to start server: %v", err)
	}
	klog.V(0).Info("Server closed")
}
