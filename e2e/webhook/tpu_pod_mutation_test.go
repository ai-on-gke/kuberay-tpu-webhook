//go:build e2e

package webhook

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
)

var (
	kubeconfig    *string
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
	testNamespace = "default"
)

const (
	// TPU webhook environment variable names.
	tpuWorkerIDEnv           = "TPU_WORKER_ID"
	tpuNameEnv               = "TPU_NAME"
	tpuDevicePluginHostIPEnv = "TPU_DEVICE_PLUGIN_HOST_IP"
	tpuDevicePluginAddrEnv   = "TPU_DEVICE_PLUGIN_ADDR"
	tpuWorkerHostnamesEnv    = "TPU_WORKER_HOSTNAMES"
	tpuProcessAddressesEnv   = "TPU_PROCESS_ADDRESSES"
	tpuProcessPortEnv        = "TPU_PROCESS_PORT"

	// Megascale environment variable names.
	megascaleSliceIDEnv            = "MEGASCALE_SLICE_ID"
	megascaleCoordinatorAddressEnv = "MEGASCALE_COORDINATOR_ADDRESS"
	megascalePortEnv               = "MEGASCALE_PORT"

	// GKE TPU-specific labels.
	replicaIndexLabelKey = "replicaIndex"

	// Ray-specific labels.
	rayNodeTypeWorker   = string(rayv1.WorkerNode)
	rayNodeTypeHead     = string(rayv1.HeadNode)
)

func init() {
	if flag.Lookup("kubeconfig") == nil {
		if home := os.Getenv("HOME"); home != "" {
			kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	if ns := os.Getenv("TEST_NAMESPACE"); ns != "" {
		testNamespace = ns
	}
	var path string
	if kubeconfig != nil && *kubeconfig != "" {
		path = *kubeconfig
	} else if home := os.Getenv("HOME"); home != "" {
		path = filepath.Join(home, ".kube", "config")
	}

	var err error

	restConfig, err = clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		log.Fatalf("Failed to load Kubernetes config (E2E tests require a pre-existing cluster; see e2e/README.md): %v", err)
	}
	clientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("Failed to create Kubernetes clientset: %v", err)
	}
	dynamicClient, err = dynamic.NewForConfig(restConfig)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}
	os.Exit(m.Run())
}

func TestWebhookMutation_V6eSingleHost(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-8-single-host.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 2)

	foundWorker := false
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			foundWorker = true
			assertCommonTPUEnvVars(t, pod.Spec.Containers[0].Env)
		}
	}
	assert.True(t, foundWorker, "No worker pods found to validate in single host topology")
}

func TestWebhookMutation_V6eMultiHost(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-16-multi-host.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 5)

	workerIds := make(map[string]bool)
	tpuNames := make(map[string]bool)
	replicaIndices := make(map[string]bool)

	numWorkerPods := 0
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			workerId := envVarValue(envVars, tpuWorkerIDEnv)
			tpuName := envVarValue(envVars, tpuNameEnv)

			assert.NotEmpty(t, workerId, tpuWorkerIDEnv+" is empty")
			assert.NotEmpty(t, tpuName, tpuNameEnv+" is empty")

			workerIds[workerId] = true
			tpuNames[tpuName] = true

			replicaIndex := pod.Labels[replicaIndexLabelKey]
			assert.NotEmpty(t, replicaIndex, replicaIndexLabelKey+" label is missing")
			replicaIndices[replicaIndex] = true

			assert.NotEmpty(t, pod.Spec.Subdomain, "Subdomain not set")
			assert.NotEmpty(t, pod.Spec.Hostname, "Hostname not set")

			assert.NotNil(t, pod.Spec.Affinity, "Affinity not set")
			assert.NotNil(t, pod.Spec.Affinity.PodAntiAffinity, "PodAntiAffinity not set")

			// Assert TPU_DEVICE_PLUGIN_HOST_IP is set via valueFrom fieldRef status.hostIP
			hostIPVarFound := false
			for _, env := range envVars {
				if env.Name == tpuDevicePluginHostIPEnv {
					hostIPVarFound = true
					assert.NotNil(t, env.ValueFrom, tpuDevicePluginHostIPEnv+" ValueFrom is nil")
					assert.NotNil(t, env.ValueFrom.FieldRef, tpuDevicePluginHostIPEnv+" FieldRef is nil")
					assert.Equal(t, "status.hostIP", env.ValueFrom.FieldRef.FieldPath, tpuDevicePluginHostIPEnv+" FieldPath is incorrect")
				}
			}
			assert.True(t, hostIPVarFound, tpuDevicePluginHostIPEnv+" environment variable missing")

			// Assert TPU_DEVICE_PLUGIN_ADDR is "$(TPU_DEVICE_PLUGIN_HOST_IP):2112"
			assert.Equal(t, fmt.Sprintf("$(%s):2112", tpuDevicePluginHostIPEnv), envVarValue(envVars, tpuDevicePluginAddrEnv), tpuDevicePluginAddrEnv+" value is incorrect")

			// Assert TPU_WORKER_HOSTNAMES matches the exact list of DNS hostnames
			numOfHosts := int(rayCluster.Spec.WorkerGroupSpecs[0].NumOfHosts)
			expectedHostnames := buildExpectedHostnames(numOfHosts, replicaIndex, rayCluster.Name)
			assert.Equal(t, expectedHostnames, envVarValue(envVars, tpuWorkerHostnamesEnv), tpuWorkerHostnamesEnv+" value is incorrect")
		}
	}

	assert.Equal(t, 4, numWorkerPods, "Expected 4 worker pods for v6e multi-host fixture")
	assert.Equal(t, 4, len(workerIds), tpuWorkerIDEnv+" values are not unique")

	for i := 0; i < 4; i++ {
		assert.True(t, workerIds[fmt.Sprint(i)], "Missing "+tpuWorkerIDEnv+" %d", i)
	}

	assert.Equal(t, 1, len(tpuNames), "All pods in the same slice should share the same "+tpuNameEnv)
	assert.Equal(t, 1, len(replicaIndices), "All pods in the same slice should share the same "+replicaIndexLabelKey)
}

func TestWebhookMutation_TorchTpuEnvs(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-8-single-host.yaml")

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	t.Logf("Looking for pods with selector: %s", labelSelector)

	pods := waitForPods(t, labelSelector, 2)

	for _, pod := range pods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			envVars := pod.Spec.Containers[0].Env
			assert.True(t, hasEnvVar(envVars, "TORCH_TPU_TOPOLOGY"), "Missing TORCH_TPU_TOPOLOGY")
			assert.True(t, hasEnvVar(envVars, "TORCH_TPU_SLICEBUILDER_ADDRESSES"), "Missing TORCH_TPU_SLICEBUILDER_ADDRESSES")

			topology := envVarValue(envVars, "TORCH_TPU_TOPOLOGY")
			assert.Equal(t, "2,4,1", topology, "Unexpected TORCH_TPU_TOPOLOGY")

			addresses := envVarValue(envVars, "TORCH_TPU_SLICEBUILDER_ADDRESSES")
			expectedAddresses := "localhost:8471,localhost:8472,localhost:8473,localhost:8474,localhost:8475,localhost:8476,localhost:8477,localhost:8478"
			assert.Equal(t, expectedAddresses, addresses, "Unexpected TORCH_TPU_SLICEBUILDER_ADDRESSES")
			break
		}
	}
}

func TestWebhookMutation_V6eMultiSlice(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-16-multi-slice.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 9)

	numWorkerPods := 0
	sliceIds := make(map[string]int)
	coordinatorAddresses := make(map[string]bool)
	megascalePorts := make(map[string]bool)

	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			assert.True(t, hasEnvVar(envVars, megascaleSliceIDEnv), "Missing "+megascaleSliceIDEnv)
			assert.True(t, hasEnvVar(envVars, megascaleCoordinatorAddressEnv), "Missing "+megascaleCoordinatorAddressEnv)
			assert.True(t, hasEnvVar(envVars, megascalePortEnv), "Missing "+megascalePortEnv)

			sliceId := envVarValue(envVars, megascaleSliceIDEnv)
			sliceIds[sliceId]++

			coordAddr := envVarValue(envVars, megascaleCoordinatorAddressEnv)
			coordinatorAddresses[coordAddr] = true

			port := envVarValue(envVars, megascalePortEnv)
			megascalePorts[port] = true
		}
	}

	assert.Equal(t, 8, numWorkerPods, "Expected 8 worker pods (2 slices of 4 hosts)")
	assert.Equal(t, 2, len(sliceIds), "Expected 2 distinct slice IDs (0 and 1)")
	assert.Equal(t, 4, sliceIds["0"], "Expected 4 worker pods in slice 0")
	assert.Equal(t, 4, sliceIds["1"], "Expected 4 worker pods in slice 1")

	assert.Equal(t, 1, len(coordinatorAddresses), "All containers in a multi-slice group should share the same coordinator address")
	assert.True(t, coordinatorAddresses["tpu-worker-group-0-0.tpu-v6e-multi-slice-headless"], "Unexpected coordinator address")

	assert.Equal(t, 1, len(megascalePorts), "All containers in a multi-slice group should share the same port configuration")
	assert.True(t, megascalePorts["8081"], "Unexpected Multi-slice Megascale port")
}

func TestWebhookMutation_V6ePodChurnSingleSlice(t *testing.T) {
	clusterName := "tpu-v6e-multi-host"
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)

	// 1. Wait for all expected pods to be created and track their names
	initialPods := waitForPods(t, labelSelector, 5)

	initialPodNames := make(map[string]bool)
	for _, p := range initialPods.Items {
		initialPodNames[p.Name] = true
	}

	// 2. Select two worker pods (half the slice) to delete concurrently
	var targetPods []corev1.Pod
	for _, pod := range initialPods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			targetPods = append(targetPods, pod)
			if len(targetPods) == 2 {
				break
			}
		}
	}

	if len(targetPods) < 2 {
		t.Fatalf("Expected at least 2 worker pods in multi-host cluster, found %d", len(targetPods))
	}

	// Record their original TPU_WORKER_IDs
	expectedWorkerIDs := make(map[string]bool)
	for _, pod := range targetPods {
		wID := envVarValue(pod.Spec.Containers[0].Env, tpuWorkerIDEnv)
		expectedWorkerIDs[wID] = true
		t.Logf("Targeting worker pod %s ("+tpuWorkerIDEnv+"=%s) for deletion", pod.Name, wID)
	}

	// 3. Delete both worker pods concurrently
	deletePodsConcurrently(t, targetPods)
	t.Log("Target pods deleted concurrently. Waiting for KubeRay operator to re-create both...")

	// 4. Poll and wait for the two brand-new worker pods to be created and mutated
	// 4. Poll and wait for the two brand-new worker pods to be created and mutated
	recreatedPods := waitForRecreatedPods(t, labelSelector, 2, initialPodNames)

	// 5. Assert that the new pods got the same TPU_WORKER_IDs as before the churn
	for _, pod := range recreatedPods {
		assignedID := envVarValue(pod.Spec.Containers[0].Env, tpuWorkerIDEnv)
		t.Logf("Re-created pod name: %s, Assigned "+tpuWorkerIDEnv+": %s", pod.Name, assignedID)
		assert.True(t, expectedWorkerIDs[assignedID], "Re-created pod "+tpuWorkerIDEnv+" %s was not in the original expected IDs", assignedID)
	}
}

func TestWebhookMutation_V6ePodChurnMultiSlice(t *testing.T) {
	clusterName := "tpu-v6e-multi-slice"
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)

	// 1. Wait for all expected pods to be created and track their names
	initialPods := waitForPods(t, labelSelector, 9)

	initialPodNames := make(map[string]bool)
	for _, p := range initialPods.Items {
		initialPodNames[p.Name] = true
	}

	// 2. Target four worker pods: two in slice 0, and two in slice 1
	var targetPods []corev1.Pod
	var slice0Targets, slice1Targets []corev1.Pod

	for _, pod := range initialPods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			sliceID := envVarValue(pod.Spec.Containers[0].Env, megascaleSliceIDEnv)
			if sliceID == "0" && len(slice0Targets) < 2 {
				slice0Targets = append(slice0Targets, pod)
			} else if sliceID == "1" && len(slice1Targets) < 2 {
				slice1Targets = append(slice1Targets, pod)
			}
		}
	}

	if len(slice0Targets) < 2 || len(slice1Targets) < 2 {
		t.Fatalf("Could not select exactly 2 target worker pods from both slice 0 and slice 1")
	}

	targetPods = append(targetPods, slice0Targets...)
	targetPods = append(targetPods, slice1Targets...)

	// Record original SliceID and WorkerID mappings
	expectedPodMappings := make(map[string]map[string]bool)
	expectedPodMappings["0"] = make(map[string]bool)
	expectedPodMappings["1"] = make(map[string]bool)

	for _, pod := range targetPods {
		sliceID := envVarValue(pod.Spec.Containers[0].Env, megascaleSliceIDEnv)
		wID := envVarValue(pod.Spec.Containers[0].Env, tpuWorkerIDEnv)
		expectedPodMappings[sliceID][wID] = true
		t.Logf("Targeting multi-slice worker pod %s (Slice=%s, "+tpuWorkerIDEnv+"=%s) for deletion", pod.Name, sliceID, wID)
	}

	// 3. Delete all four worker pods concurrently
	deletePodsConcurrently(t, targetPods)
	t.Log("Target pods deleted concurrently. Polling and triggering KubeRay operator reconciliation until recreation starts...")

	// Wait for KubeRay Operator to satisfy its informer expectations and start recreation
	err := wait.PollUntilContextTimeout(t.Context(), 4*time.Second, 45*time.Second, true, func(ctx context.Context) (bool, error) {
		triggerRayClusterReconcile(t, clusterName)
		currentPods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		for _, p := range currentPods.Items {
			if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker && !initialPodNames[p.Name] {
				t.Log("Recreation has successfully started!")
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for KubeRay operator to start recreating pods: %v", err)
	}

	t.Log("Recreation started. Waiting for KubeRay operator to fully re-create and mutate all four Pods...")

	// 4. Poll and wait for all four new worker pods to be created and mutated
	recreatedPods := waitForRecreatedPods(t, labelSelector, 4, initialPodNames)

	// 5. Assert that each recreated pod got its original TPU_WORKER_ID under the correct MEGASCALE_SLICE_ID
	for _, pod := range recreatedPods {
		sliceID := envVarValue(pod.Spec.Containers[0].Env, megascaleSliceIDEnv)
		assignedID := envVarValue(pod.Spec.Containers[0].Env, tpuWorkerIDEnv)
		t.Logf("Re-created multi-slice pod name: %s, Assigned Slice: %s, "+tpuWorkerIDEnv+": %s", pod.Name, sliceID, assignedID)

		originalExpectedIDs, exists := expectedPodMappings[sliceID]
		assert.True(t, exists, "Recreated pod assigned to unexpected sliceID: %s", sliceID)
		assert.True(t, originalExpectedIDs[assignedID], "Recreated pod in slice %s with "+tpuWorkerIDEnv+" %s was not in the original expected set", sliceID, assignedID)
	}
}

func loadManifest(t *testing.T, relativePath string) *rayv1.RayCluster {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("Failed to get current file path via runtime.Caller")
	}
	manifestPath := filepath.Clean(filepath.Join(filepath.Dir(filename), relativePath))
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("Error opening manifest file at %s: %v", manifestPath, err)
	}
	defer manifestFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(manifestFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding manifest at %s: %v", manifestPath, err)
	}
	return &rayCluster
}

func waitForPods(t *testing.T, labelSelector string, expectedCount int) *corev1.PodList {
	t.Helper()
	var pods *corev1.PodList
	err := wait.PollUntilContextTimeout(t.Context(), 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		pods, err = clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		if len(pods.Items) >= expectedCount {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Error waiting for %d pods with selector %s: %v (found %d pods)", expectedCount, labelSelector, err, len(pods.Items))
	}
	return pods
}

func TestWebhookMutation_HeterogeneousCluster(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/v6e/heterogeneous-cluster.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 3)

	numTpuWorkers := 0
	numCpuWorkers := 0

	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			if pod.Labels[utils.RayNodeGroupLabelKey] == "tpu-worker-group" {
				numTpuWorkers++
				envVars := pod.Spec.Containers[0].Env
				assert.True(t, hasEnvVar(envVars, tpuWorkerIDEnv), "TPU worker missing "+tpuWorkerIDEnv)
				assert.NotEmpty(t, pod.Labels[replicaIndexLabelKey], "TPU worker missing "+replicaIndexLabelKey+" label")
			} else if pod.Labels[utils.RayNodeGroupLabelKey] == "cpu-worker-group" {
				numCpuWorkers++
				envVars := pod.Spec.Containers[0].Env
				assert.False(t, hasEnvVar(envVars, tpuWorkerIDEnv), "CPU worker erroneously mutated with "+tpuWorkerIDEnv)
				assert.Empty(t, pod.Labels[replicaIndexLabelKey], "CPU worker erroneously mutated with "+replicaIndexLabelKey+" label")
				assert.Empty(t, pod.Spec.Subdomain, "CPU worker subdomain should be empty")
				assert.Empty(t, pod.Spec.Hostname, "CPU worker hostname should be empty")
				assert.Nil(t, pod.Spec.Affinity, "CPU worker affinity should be nil")
			}
		}
	}

	assert.Equal(t, 1, numTpuWorkers, "Expected exactly 1 TPU worker pod")
	assert.Equal(t, 1, numCpuWorkers, "Expected exactly 1 CPU worker pod")
}

func TestWebhookMutation_V7xSingleHost(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/tpu7x/tpu7x-8-single-host.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 2)

	foundWorker := false
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			foundWorker = true
			envVars := pod.Spec.Containers[0].Env
			assertCommonTPUEnvVars(t, envVars)
			assert.Equal(t, "0", envVarValue(envVars, tpuWorkerIDEnv), tpuWorkerIDEnv+" is incorrect")
		}
	}
	assert.True(t, foundWorker, "No worker pods found to validate in V7x single host topology")
}

func TestWebhookMutation_V7xMultiHost(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/tpu7x/tpu7x-16-multi-host.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 3)

	workerIds := make(map[string]bool)
	tpuNames := make(map[string]bool)

	numWorkerPods := 0
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			workerId := envVarValue(envVars, tpuWorkerIDEnv)
			tpuName := envVarValue(envVars, tpuNameEnv)

			assert.NotEmpty(t, workerId, tpuWorkerIDEnv+" is empty")
			assert.NotEmpty(t, tpuName, tpuNameEnv+" is empty")

			workerIds[workerId] = true
			tpuNames[tpuName] = true

			assert.NotEmpty(t, pod.Spec.Subdomain, "Subdomain not set")
			assert.NotEmpty(t, pod.Spec.Hostname, "Hostname not set")

			replicaIndex := pod.Labels[replicaIndexLabelKey]
			assert.NotEmpty(t, replicaIndex, replicaIndexLabelKey+" label is missing")

			// Assert v7x-specific process address variables dynamically
			numOfHosts := int(rayCluster.Spec.WorkerGroupSpecs[0].NumOfHosts)
			// Determine number of TPU containers per pod from manifest resource requests (google.com/tpu: "4")
			// We'll assume 1 TPU container per pod since requests: 4 for tpu7x (which is a dual-chiplet host with 4 chips total).
			numTpuContainers := 1
			expectedAddresses := buildExpectedProcessAddresses(numOfHosts, replicaIndex, rayCluster.Name, numTpuContainers)
			assert.Equal(t, expectedAddresses, envVarValue(envVars, tpuProcessAddressesEnv), tpuProcessAddressesEnv+" value is incorrect")
			assert.Equal(t, "8471", envVarValue(envVars, tpuProcessPortEnv), tpuProcessPortEnv+" value is incorrect")
		}
	}

	assert.Equal(t, 2, numWorkerPods, "Expected 2 worker pods for v7x multi-host fixture")
	assert.Equal(t, 2, len(workerIds), tpuWorkerIDEnv+" values are not unique")
	assert.True(t, workerIds["0"], "Missing "+tpuWorkerIDEnv+" 0")
	assert.True(t, workerIds["1"], "Missing "+tpuWorkerIDEnv+" 1")
	assert.Equal(t, 1, len(tpuNames), "All pods in the same slice should share the same "+tpuNameEnv)
}

func hasEnvVar(envVars []corev1.EnvVar, name string) bool {
	for _, env := range envVars {
		if env.Name == name {
			return true
		}
	}
	return false
}

func envVarValue(envVars []corev1.EnvVar, name string) string {
	for _, env := range envVars {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

func deletePodsConcurrently(t *testing.T, pods []corev1.Pod) {
	t.Helper()
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			err := clientset.CoreV1().Pods(testNamespace).Delete(t.Context(), podName, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("Failed to delete pod %s: %v", podName, err)
				return
			}

			// Wait for the pod to be completely removed from the API server (graceful cleanup finished)
			err = wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 60*time.Second, true, func(ctx context.Context) (bool, error) {
				_, err := clientset.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					if apierrors.IsNotFound(err) {
						return true, nil
					}
					return false, err
				}
				return false, nil
			})
			if err != nil {
				t.Errorf("Pod %s was not fully deleted within 60s: %v", podName, err)
			}
		}(pod.Name)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func buildExpectedHostnames(numOfHosts int, replicaIndexLabelVal string, clusterName string) string {
	headlessService := fmt.Sprintf("%s-headless", clusterName)
	hostnames := make([]string, numOfHosts)
	for j := 0; j < numOfHosts; j++ {
		hostnames[j] = fmt.Sprintf("%s-%d.%s", replicaIndexLabelVal, j, headlessService)
	}
	return strings.Join(hostnames, ",")
}

func buildExpectedProcessAddresses(numOfHosts int, replicaIndexLabelVal string, clusterName string, numTpuContainers int) string {
	headlessService := fmt.Sprintf("%s-headless", clusterName)
	var addresses []string
	for h := 0; h < numOfHosts; h++ {
		hostName := fmt.Sprintf("%s-%d.%s", replicaIndexLabelVal, h, headlessService)
		for c := 0; c < numTpuContainers; c++ {
			port := 8471 + c
			addresses = append(addresses, fmt.Sprintf("%s:%d", hostName, port))
		}
	}
	return strings.Join(addresses, ",")
}

func TestWebhookMutation_V7xMultiContainer(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/tpu7x/tpu7x-multi-container.yaml")

	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, rayCluster.Name)
	t.Logf("Looking for pods with selector: %s", labelSelector)

	pods := waitForPods(t, labelSelector, 2)

	foundWorker := false
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			foundWorker = true
			assert.Equal(t, 2, len(pod.Spec.Containers), "Expected Pod to have exactly 2 containers")

			// Container 0 gets TPU_WORKER_ID 0 and base process port 8471
			envVars0 := pod.Spec.Containers[0].Env
			assert.Equal(t, "0", envVarValue(envVars0, tpuWorkerIDEnv), "Container 0 "+tpuWorkerIDEnv+" is incorrect")
			assert.Equal(t, "8471", envVarValue(envVars0, tpuProcessPortEnv), "Container 0 "+tpuProcessPortEnv+" is incorrect")

			// Container 1 gets TPU_WORKER_ID 1 and consecutive process port 8472
			envVars1 := pod.Spec.Containers[1].Env
			assert.Equal(t, "1", envVarValue(envVars1, tpuWorkerIDEnv), "Container 1 "+tpuWorkerIDEnv+" is incorrect")
			assert.Equal(t, "8472", envVarValue(envVars1, tpuProcessPortEnv), "Container 1 "+tpuProcessPortEnv+" is incorrect")
		}
	}
	assert.True(t, foundWorker, "No worker pods found to validate in V7x multi-container topology")
}

func TestWebhookMutation_V7xMultiSlice(t *testing.T) {
	rayCluster := loadManifest(t, "../manifests/tpu7x/tpu7x-16-multi-slice.yaml")

	labelSelector := getLabelSelector(t, rayCluster.Name)

	pods := waitForPods(t, labelSelector, 5)

	numWorkerPods := 0
	sliceIds := make(map[string]int)
	coordinatorAddresses := make(map[string]bool)

	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			assert.True(t, hasEnvVar(envVars, megascaleSliceIDEnv), "Missing "+megascaleSliceIDEnv)
			assert.True(t, hasEnvVar(envVars, megascaleCoordinatorAddressEnv), "Missing "+megascaleCoordinatorAddressEnv)
			assert.True(t, hasEnvVar(envVars, megascalePortEnv), "Missing "+megascalePortEnv)

			sliceId := envVarValue(envVars, megascaleSliceIDEnv)
			sliceIds[sliceId]++

			coordAddr := envVarValue(envVars, megascaleCoordinatorAddressEnv)
			coordinatorAddresses[coordAddr] = true
		}
	}

	assert.Equal(t, 4, numWorkerPods, "Expected 4 worker pods (2 slices of 2 hosts)")
	assert.Equal(t, 2, len(sliceIds), "Expected 2 distinct slice IDs")
	assert.Equal(t, 2, sliceIds["0"], "Expected 2 worker pods in slice 0")
	assert.Equal(t, 2, sliceIds["1"], "Expected 2 worker pods in slice 1")

	assert.Equal(t, 1, len(coordinatorAddresses), "All containers in a multi-slice group should share the same coordinator address")
	assert.True(t, coordinatorAddresses["tpu-worker-group-0-0.tpu-7x-multi-slice-headless:8081"], "Unexpected coordinator address")
}

func TestWebhookMutation_V6eDNSResolution(t *testing.T) {
	clusterName := "tpu-v6e-multi-host"
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)

	pods, err := clientset.CoreV1().Pods(testNamespace).List(t.Context(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(pods.Items) == 0 {
		t.Skip("Skipping DNS resolution test: No multi-host pods found in default namespace.")
	}

	var workerPod *corev1.Pod
	for _, p := range pods.Items {
		if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
			workerPod = &p
			break
		}
	}

	if workerPod == nil {
		t.Fatalf("No TPU worker pods found in cluster %s", clusterName)
	}

	// Wait for all worker pods to be in Running phase so DNS endpoints are fully registered
	t.Log("Waiting for all worker pods to reach Running phase...")
	err = wait.PollUntilContextTimeout(t.Context(), 5*time.Second, 240*time.Second, true, func(ctx context.Context) (bool, error) {
		currentPods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}

		allRunning := true
		workerCount := 0
		for _, p := range currentPods.Items {
			if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
				workerCount++
				if p.Status.Phase != corev1.PodRunning {
					allRunning = false
					break
				}
				// Ensure container status is also running
				containerRunning := false
				for _, cs := range p.Status.ContainerStatuses {
					if cs.Name == p.Spec.Containers[0].Name && cs.State.Running != nil {
						containerRunning = true
						break
					}
				}
				if !containerRunning {
					allRunning = false
					break
				}
			}
		}
		return allRunning && workerCount > 0, nil
	})
	if err != nil {
		t.Fatalf("Not all worker pods reached Running phase within 120s: %v", err)
	}

	// Wait for CoreDNS/Kube-DNS propagation to fully sync endpoints
	t.Log("All worker pods are Running. Waiting 5 seconds for DNS propagation...")
	time.Sleep(5 * time.Second)

	// Refresh target workerPod reference to ensure status reflects Running state
	p, err := clientset.CoreV1().Pods(testNamespace).Get(t.Context(), workerPod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to refresh worker pod status: %v", err)
	}
	workerPod = p

	envVars := workerPod.Spec.Containers[0].Env
	hostnamesStr := envVarValue(envVars, tpuWorkerHostnamesEnv)
	assert.NotEmpty(t, hostnamesStr, tpuWorkerHostnamesEnv+" env var is missing or empty")

	hostnames := strings.Split(hostnamesStr, ",")
	for _, hostname := range hostnames {
		t.Logf("Attempting to resolve hostname %s from inside pod %s...", hostname, workerPod.Name)
		stdout, stderr, err := execCommandInPod(t, workerPod.Name, workerPod.Spec.Containers[0].Name, []string{"getent", "hosts", hostname})
		if err != nil {
			t.Errorf("Failed to resolve hostname %s inside container: %v (stderr: %q, stdout: %q)", hostname, err, stderr, stdout)
		} else {
			t.Logf("Successfully resolved %s: %s", hostname, strings.TrimSpace(stdout))
		}
	}
}

func execCommandInPod(t *testing.T, podName string, containerName string, cmd []string) (string, string, error) {
	t.Helper()
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(testNamespace).
		SubResource("exec")
	option := &corev1.PodExecOptions{
		Container: containerName,
		Command:   cmd,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
	req.VersionedParams(option, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(t.Context(), remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

func writeLocalFileToPod(t *testing.T, podName string, containerName string, localPath string, remotePath string) {
	t.Helper()
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("Failed to read local file %s: %v", localPath, err)
	}
	t.Logf("Writing local file %s to %s in pod %s...", localPath, remotePath, podName)
	cmd := []string{"bash", "-c", "cat << 'EOF' > " + remotePath + "\n" + string(content) + "\nEOF\n"}
	stdout, stderr, err := execCommandInPod(t, podName, containerName, cmd)
	if err != nil {
		t.Fatalf("Failed to write file %s to pod: %v (stdout: %q, stderr: %q)", remotePath, err, stdout, stderr)
	}
}



func waitForAllPodsRunning(t *testing.T, clusterName string, expectedWorkerCount int, timeout time.Duration) *corev1.Pod {
	t.Helper()
	t.Logf("Waiting for head pod and all %d worker pods of cluster %s to reach Running phase...", expectedWorkerCount, clusterName)
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)
	var headPod *corev1.Pod
	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}

		headRunning := false
		workerRunningCount := 0
		for _, p := range pods.Items {
			if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeHead && p.Status.Phase == corev1.PodRunning {
				headRunning = true
				pCopy := p
				headPod = &pCopy
			} else if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
				if p.Status.Phase == corev1.PodRunning {
					containerRunning := false
					for _, cs := range p.Status.ContainerStatuses {
						if cs.Name == p.Spec.Containers[0].Name && cs.State.Running != nil {
							containerRunning = true
							break
						}
					}
					if containerRunning {
						workerRunningCount++
					}
				}
			}
		}
		t.Logf("Checking pod states... Head Running: %t, Workers Running: %d/%d", headRunning, workerRunningCount, expectedWorkerCount)
		return headRunning && workerRunningCount == expectedWorkerCount, nil
	})
	if err != nil {
		t.Fatalf("Pods of cluster %s failed to reach Running phase within %v: %v", clusterName, timeout, err)
	}
	return headPod
}

func triggerRayClusterReconcile(t *testing.T, clusterName string) {
	t.Helper()
	gvr := schema.GroupVersionResource{
		Group:    "ray.io",
		Version:  "v1",
		Resource: "rayclusters",
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cluster, err := dynamicClient.Resource(gvr).Namespace(testNamespace).Get(t.Context(), clusterName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		annotations := cluster.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations["tpu-webhook.gke.io/reconcile-trigger"] = fmt.Sprint(time.Now().UnixNano())
		cluster.SetAnnotations(annotations)

		_, err = dynamicClient.Resource(gvr).Namespace(testNamespace).Update(t.Context(), cluster, metav1.UpdateOptions{})
		return err
	})

	if err != nil {
		t.Fatalf("Failed to trigger reconciliation for RayCluster %s: %v", clusterName, err)
	}
	t.Logf("Successfully triggered immediate KubeRay reconciliation for %s", clusterName)
}

func getLabelSelector(t *testing.T, clusterName string) string {
	t.Helper()
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)
	t.Logf("Looking for pods with selector: %s", labelSelector)
	return labelSelector
}

func assertCommonTPUEnvVars(t *testing.T, envVars []corev1.EnvVar) {
	t.Helper()
	assert.True(t, hasEnvVar(envVars, tpuWorkerIDEnv), "Missing "+tpuWorkerIDEnv)
	assert.NotEmpty(t, envVarValue(envVars, tpuWorkerIDEnv), tpuWorkerIDEnv+" is empty")

	assert.True(t, hasEnvVar(envVars, tpuNameEnv), "Missing "+tpuNameEnv)
	assert.NotEmpty(t, envVarValue(envVars, tpuNameEnv), tpuNameEnv+" is empty")

	assert.True(t, hasEnvVar(envVars, tpuDevicePluginHostIPEnv), "Missing "+tpuDevicePluginHostIPEnv)
	assert.True(t, hasEnvVar(envVars, tpuDevicePluginAddrEnv), "Missing "+tpuDevicePluginAddrEnv)
}

func waitForRecreatedPods(t *testing.T, labelSelector string, expectedCount int, initialPodNames map[string]bool) []corev1.Pod {
	t.Helper()
	var recreatedPods []corev1.Pod
	err := wait.PollUntilContextTimeout(t.Context(), 3*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		currentPods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}

		recreatedPods = nil
		for _, p := range currentPods.Items {
			if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker && !initialPodNames[p.Name] {
				if hasEnvVar(p.Spec.Containers[0].Env, tpuWorkerIDEnv) {
					recreatedPods = append(recreatedPods, p)
				}
			}
		}
		if len(recreatedPods) == expectedCount {
			return true, nil
		}
		t.Logf("Waiting for recreated pods... (found %d/%d)", len(recreatedPods), expectedCount)
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for recreated pods: %v (found %d/%d)", err, len(recreatedPods), expectedCount)
	}
	return recreatedPods
}

func TestWebhookIntegration_RayTPUUtilsAndJAX(t *testing.T) {
	clusterName := "tpu-v6e-integration"

	// Wait for head pod and both TPU worker pods to reach Running phase
	headPod := waitForAllPodsRunning(t, clusterName, 2, 240*time.Second)

	// Write utility verification script into the head pod
	writeLocalFileToPod(t, headPod.Name, headPod.Spec.Containers[0].Name, "../scripts/verify_tpu_utils.py", "/tmp/verify_tpu_utils.py")

	// Execute verify_tpu_utils.py via Python inside the head pod
	t.Log("Running verify_tpu_utils.py E2E verification workload...")
	runCmd := []string{"python3", "/tmp/verify_tpu_utils.py"}
	stdout, stderr, err := execCommandInPod(t, headPod.Name, headPod.Spec.Containers[0].Name, runCmd)
	if err != nil {
		t.Fatalf("TPU utilities and JAX verification failed: %v (stdout: %q, stderr: %q)", err, stdout, stderr)
	}

	t.Logf("Execution output:\n%s", stdout)
	assert.Contains(t, stdout, "All Ray core TPU utilities and JAX/XLA distributed inits verified successfully.")
}
