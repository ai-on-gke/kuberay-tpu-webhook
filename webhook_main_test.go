package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/pointer"
)

// getTestCPUWorker returns a template for a Ray Pod that requests CPUs.
func getTestCPUWorker(clusterName string, groupName string, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cpu-pod",
			Namespace: namespace,
			Labels: map[string]string{
				utils.RayNodeLabelKey:      "yes",
				utils.RayClusterLabelKey:   clusterName,
				utils.RayNodeGroupLabelKey: groupName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "ray-worker",
				},
			},
			NodeSelector: map[string]string{},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "ray-worker",
					State: corev1.ContainerState{},
				},
			},
		},
	}
}

// getTestTPUWorker returns template for a TPU Ray worker pod
func getTestTPUWorker(clusterName string, groupName string, namespace string, accelerator string, topology string, tpuResource string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "tpu-pod",
			Namespace: namespace,
			Labels: map[string]string{
				utils.RayNodeLabelKey:      "yes",
				utils.RayClusterLabelKey:   clusterName,
				utils.RayNodeGroupLabelKey: groupName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "ray-worker",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"google.com/tpu": resource.MustParse(tpuResource),
						},
						Requests: corev1.ResourceList{
							"google.com/tpu": resource.MustParse(tpuResource),
						},
					},
					Env: []corev1.EnvVar{},
				},
			},
			NodeSelector: map[string]string{
				"cloud.google.com/gke-tpu-accelerator": accelerator,
				"cloud.google.com/gke-tpu-topology":    topology,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "ray-worker",
					State: corev1.ContainerState{},
				},
			},
		},
	}
}

// getTestPods returns a list of Ray Pods based on the provided worker template.
func getTestPods(templatePod *corev1.Pod, clusterName string, namespace string, numPods int) []*corev1.Pod {
	testPods := []*corev1.Pod{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "headNode",
				Namespace: namespace,
				Labels: map[string]string{
					utils.RayNodeLabelKey:      "yes",
					utils.RayClusterLabelKey:   clusterName,
					utils.RayNodeTypeLabelKey:  string(rayv1.HeadNode),
					utils.RayNodeGroupLabelKey: "head-group",
				},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:    "ray-head",
						Image:   "rayproject/autoscaler",
						Command: []string{"python"},
						Args:    []string{"/opt/code.py"},
					},
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "1.2.3.4",
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name:  "ray-head",
						State: corev1.ContainerState{},
					},
				},
			},
		},
	}
	// add numPods worker pods with unique names
	for i := 0; i < numPods; i++ {
		templatePodCopy := templatePod.DeepCopy()
		templatePodCopy.Name = fmt.Sprintf("%s-%d", templatePod.Name, i)
		testPods = append(testPods, templatePodCopy)
	}
	return testPods
}

// getTestInterceptedTPUPods returns numOfHosts * numSlices TPU worker pods with env vars set
func getTestInterceptedTPUPods(templatePod *corev1.Pod, numPods int, numSlices int, numOfHosts int) []*corev1.Pod {
	testInterceptedTPUPods := []*corev1.Pod{}
	hostnames := ""
	for i := 0; i < numPods; i++ {
		replicaID := i / numOfHosts
		workerID := i % numOfHosts

		// generate new batch of hostnames for slice
		if workerID == 0 {
			tempHostNames := make([]string, numOfHosts)
			groupName := templatePod.Labels[utils.RayNodeGroupLabelKey]
			for ind := 0; ind < numOfHosts; ind++ {
				tempHostNames[ind] = fmt.Sprintf("%s-%d-%d", groupName, replicaID, workerID)
			}
			hostnames = strings.Join(tempHostNames, ",")
		}

		// set fields for new Pod
		testTPUWorkerCopy := templatePod.DeepCopy()
		groupName := templatePod.Labels[utils.RayNodeGroupLabelKey]
		replicaIndex := fmt.Sprintf("%s-%d", groupName, replicaID)
		env := []corev1.EnvVar{
			{
				Name:  "TPU_WORKER_ID",
				Value: fmt.Sprint(workerID),
			},
			{
				Name:  "TPU_WORKER_HOSTNAMES",
				Value: hostnames,
			},
			{
				Name:  "TPU_NAME",
				Value: replicaIndex,
			},
			{
				Name: "TPU_DEVICE_PLUGIN_HOST_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "status.hostIP",
					},
				},
			},
			{
				Name:  "TPU_DEVICE_PLUGIN_ADDR",
				Value: "$(TPU_DEVICE_PLUGIN_HOST_IP):2112",
			},
		}
		testTPUWorkerCopy.Spec.Containers[0].Env = env
		testTPUWorkerCopy.Name = fmt.Sprintf("%s-%d", "intercepted-tpu-pod", i)
		testTPUWorkerCopy.Labels["replicaIndex"] = replicaIndex
		testInterceptedTPUPods = append(testInterceptedTPUPods, testTPUWorkerCopy)
	}
	return testInterceptedTPUPods
}

func getTestTPUWorkerGroup(groupName string, numOfHosts int32, numReplicas int32, accelerator string, topology string, tpuResource string) *rayv1.WorkerGroupSpec {
	return &rayv1.WorkerGroupSpec{
		Replicas:    pointer.Int32(numReplicas),
		MinReplicas: pointer.Int32(0),
		MaxReplicas: pointer.Int32(10000),
		NumOfHosts:  numOfHosts,
		GroupName:   groupName,
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name: "ray-worker",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								"google.com/tpu": resource.MustParse(tpuResource),
							},
							Requests: corev1.ResourceList{
								"google.com/tpu": resource.MustParse(tpuResource),
							},
						},
						Env: []corev1.EnvVar{
							{
								Name: "MY_POD_IP",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "status.podIP",
									},
								},
							},
						},
					},
				},
				NodeSelector: map[string]string{
					"cloud.google.com/gke-tpu-accelerator": accelerator,
					"cloud.google.com/gke-tpu-topology":    topology,
				},
			},
		},
	}
}

func getTestAdmissionReview(kind string, operation string) *admissionv1.AdmissionReview {
	return &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "1",
			Kind: metav1.GroupVersionKind{
				Kind: kind,
			},
			Operation: admissionv1.Operation(operation),
			// set these values inside test
			Object: runtime.RawExtension{
				Raw:    nil,
				Object: nil,
			},
			OldObject: runtime.RawExtension{
				Raw:    nil,
				Object: nil,
			},
		},
	}
}

// getTestRayCluster returns a RayCluster manifest with a TPU worker group
func getTestRayCluster(clusterName string, groupName string, namespace string, numOfHosts int32, numReplicas int32, tpuResource string, accelerator string, topology string, enableGPU bool) *rayv1.RayCluster {
	rayCluster := &rayv1.RayCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: rayv1.RayClusterSpec{
			HeadGroupSpec: rayv1.HeadGroupSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "ray-head",
							},
						},
					},
				},
			},
			WorkerGroupSpecs: []rayv1.WorkerGroupSpec{
				{
					Replicas:    pointer.Int32(numReplicas),
					MinReplicas: pointer.Int32(0),
					MaxReplicas: pointer.Int32(10000),
					NumOfHosts:  numOfHosts,
					GroupName:   groupName,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name: "ray-worker",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											"cpu":            resource.MustParse("1"),
											"google.com/tpu": resource.MustParse(tpuResource),
										},
										Limits: corev1.ResourceList{
											"cpu":            resource.MustParse("1"),
											"google.com/tpu": resource.MustParse(tpuResource),
										},
									},
									Env: []corev1.EnvVar{
										{
											Name: "MY_POD_IP",
											ValueFrom: &corev1.EnvVarSource{
												FieldRef: &corev1.ObjectFieldSelector{
													FieldPath: "status.podIP",
												},
											},
										},
									},
								},
							},
							NodeSelector: map[string]string{
								"cloud.google.com/gke-tpu-accelerator": accelerator,
								"cloud.google.com/gke-tpu-topology":    topology,
							},
						},
					},
				},
			},
		},
	}

	if enableGPU {
		gpuGroup := rayv1.WorkerGroupSpec{
			Replicas:    pointer.Int32(numReplicas),
			MinReplicas: pointer.Int32(0),
			MaxReplicas: pointer.Int32(10000),
			NumOfHosts:  1,
			GroupName:   "gpu-group",
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "ray-worker",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									"cpu":            resource.MustParse("1"),
									"nvidia.com/gpu": resource.MustParse("4"),
								},
								Limits: corev1.ResourceList{
									"cpu":            resource.MustParse("1"),
									"nvidia.com/gpu": resource.MustParse("4"),
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "MY_POD_IP",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "status.podIP",
										},
									},
								},
							},
						},
					},
					NodeSelector: map[string]string{},
				},
			},
		}
		rayCluster.Spec.WorkerGroupSpecs = append(rayCluster.Spec.WorkerGroupSpecs, gpuGroup)
	}

	return rayCluster
}

// setupInformer creates a PodInformer, waits for cache to sync, and returns the Informer PodLister
func setupInformer(pods ...*corev1.Pod) listersv1.PodLister {
	// initialize fake Clientset with pod objects
	tpuObjects := make([]runtime.Object, len(pods))
	for i, pod := range pods {
		tpuObjects[i] = pod
	}
	fakeClientSet := fake.NewSimpleClientset(tpuObjects...)

	// initialize podLister using the fake client for testing
	factory := informers.NewSharedInformerFactory(fakeClientSet, 0)
	podInformer := factory.Core().V1().Pods().Informer()

	stopCh := make(chan struct{})
	defer close(stopCh)

	factory.Start(stopCh)
	factory.WaitForCacheSync(stopCh)

	// wait for cache to sync before creating the Lister
	if !cache.WaitForCacheSync(stopCh, podInformer.HasSynced) {
		fmt.Printf("Timed out waiting for fake client to sync")
		return nil
	}

	return factory.Core().V1().Pods().Lister()
}

func Test_GetReplicaIndex(t *testing.T) {
	tests := map[string]struct {
		sliceToWorkerIDs     map[slice][]int
		expectedReplicaIndex int
	}{
		"nil sliceToWorkerIDs": {
			// defaults to assigning Pod to replica 0
			sliceToWorkerIDs:     nil,
			expectedReplicaIndex: 0,
		},
		"empty sliceToWorkerIDs": {
			// should assign Pod to replica 0 since no other Pods in slice
			sliceToWorkerIDs:     make(map[slice][]int),
			expectedReplicaIndex: 0,
		},
		"single-host worker group missing worker": {
			// should assign Pod to replica 0 since # workers < 1 for that slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)}: []int{},
			},
			expectedReplicaIndex: 0,
		},
		"single-host worker group with all workers created": {
			// should assign Pod to replica 1 since one existing slice with all workers created
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)}: []int{0},
			},
			expectedReplicaIndex: 1,
		},
		"multi-host worker group missing worker": {
			// should assign Pod to replica 0 since # workers < 4 for that slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2},
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)}: []int{0, 1, 2, 3},
			},
			expectedReplicaIndex: 0,
		},
		"multi-host worker group with all workers created": {
			// should assign Pod to replica 1 since one existing slice with all workers created
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2, 3},
			},
			expectedReplicaIndex: 1,
		},
		"multi-slice worker group": {
			// should assign Pod to replica 4 since 3 existing slices with all workers created
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 2, int32(4)}: []int{0, 1, 2, 3},
			},
			expectedReplicaIndex: 3,
		},
	}

	// validate getReplicaIndex() returns the expected Replica ID for TPU pods in varying pod slices
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			replicaIndex := getReplicaIndex(tc.sliceToWorkerIDs, "test-cluster", "test-group", "test-namespace")
			assert.Equal(t, tc.expectedReplicaIndex, replicaIndex)
		})
	}
}

func Test_GetNextWorkerID(t *testing.T) {
	tests := map[string]struct {
		sliceToWorkerIDs    map[slice][]int
		podSlice            slice
		replicaIndex        int
		expectedError       error
		expectedTPUWorkerID int
	}{
		"nil sliceToWorkerIDs": {
			// defaults to assigning Pod to TPU_WORKER_ID=0
			sliceToWorkerIDs:    nil,
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)},
			replicaIndex:        0,
			expectedTPUWorkerID: 0,
		},
		"empty sliceToWorkerIDs": {
			// should assign Pod to TPU_WORKER_ID=0 since no other Pods in slice
			sliceToWorkerIDs:    make(map[slice][]int),
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)},
			replicaIndex:        0,
			expectedTPUWorkerID: 0,
		},
		"single-host worker group with empty worker ID list": {
			// should assign Pod to TPU_WORKER_ID=0 since # workers < 1 for that slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)}: []int{},
			},
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 0, int32(1)},
			replicaIndex:        0,
			expectedTPUWorkerID: 0,
		},
		"multi-host worker group with deleted worker": {
			// should assign Pod to TPU_WORKER_ID=2 since that's the next lowest int ID in the slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{3, 0, 1},
			},
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)},
			replicaIndex:        0,
			expectedTPUWorkerID: 2,
		},
		"multi-host worker group with # worker IDs < NumOfHosts": {
			// should assign Pod to TPU_WORKER_ID=3 since that's the next lowest int ID in the slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)}: []int{0, 1, 2},
			},
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)},
			replicaIndex:        1,
			expectedTPUWorkerID: 3,
		},
		"multi-host worker group with incorrectly assigned worker IDs": {
			// should error since two or more Pods in a slice have identical TPU_WORKER_IDs
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)}: []int{0, 1, 1},
			},
			podSlice:      slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)},
			replicaIndex:  1,
			expectedError: errors.New("Identical TPU_WORKER_ID assigned to multiple TPU workers in slice"),
		},
		"multi-slice worker group with all workers created": {
			// should always assign Pod to TPU_WORKER_ID=0 in a new slice
			sliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(4)}: []int{0, 1, 2, 3},
				slice{"test-cluster", "test-group", "test-namespace", 2, int32(4)}: []int{0, 1, 2, 3},
			},
			podSlice:            slice{"test-cluster", "test-group", "test-namespace", 3, int32(4)},
			replicaIndex:        3,
			expectedTPUWorkerID: 0,
		},
	}

	// validate getNextWorkerID() returns the expected TPU_WORKER ID for different sliceToWorkerIDs
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			workerID, err := getNextWorkerID(tc.sliceToWorkerIDs, tc.podSlice, "test-namespace", tc.replicaIndex)
			if err != nil {
				assert.Equal(t, tc.expectedError, err)
			}
			assert.Equal(t, tc.expectedTPUWorkerID, workerID)
		})
	}
}

func Test_ContainerRequestingTPUs(t *testing.T) {
	tests := map[string]struct {
		testPod      *corev1.Pod
		numPods      int
		requestsTPUs bool
	}{
		"Check for containerRequestingTPUs in CPU pods": {
			// no TPUs requested - should all be false
			testPod:      getTestCPUWorker("test-cluster", "test-group", "test-namespace"),
			numPods:      4,
			requestsTPUs: false,
		},
		"Check for containerRequestingTPUs in TPU pods": {
			// TPUs requested - should all be true for worker pod containers
			testPod:      getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x2", "4"),
			numPods:      4,
			requestsTPUs: true,
		},
	}

	// check containerRequestingTPUs returns true when a container requests google.com/tpu resources
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			testPods := getTestPods(tc.testPod, "test-cluster", "test-namespace", tc.numPods)
			for _, pod := range testPods {
				if pod.Labels[utils.RayNodeTypeLabelKey] == string(rayv1.WorkerNode) {
					assert.Equal(t, tc.requestsTPUs, containerRequestingTPUs(pod.Spec.Containers...))
				}
			}
		})
	}
}

func Test_GetNumTPUHostsFromTopology(t *testing.T) {
	tests := map[string]struct {
		topology      string
		chipsPerHost  int64
		expectedHosts int32
		expectedError error
	}{
		"getNumTPUHostsFromTopology with empty topology": {
			// empty gke-tpu-topology - returns error
			topology:      "",
			expectedHosts: int32(0),
			expectedError: errors.New("TPU topology not specified"),
		},
		"getNumTPUHostsFromTopology with v4 2x2x1 topology": {
			// v4 - 2x2x1, 4 chips per host, should return 1 TPU VM
			topology:      "2x2x1",
			expectedHosts: int32(1),
			chipsPerHost:  int64(4),
		},
		"getNumTPUHostsFromTopology with v4 2x2x4 topology": {
			// v4 - 2x2x4, 4 chips per host, should return 4 TPU VMs
			topology:      "2x2x4",
			expectedHosts: int32(4),
			chipsPerHost:  int64(4),
		},
		"getNumTPUHostsFromTopology with v5litepod-4 2x4 topology": {
			// v5e - 2x4 and 4 chips per VM, should return 2 TPU VMs
			topology:      "2x4",
			expectedHosts: int32(2),
			chipsPerHost:  int64(4),
		},
		"getNumTPUHostsFromTopology with v5litepod-8 2x4 topology": {
			// v5e - 2x4 and 8 chips per VM, should return 1 TPU VM
			topology:      "2x4",
			expectedHosts: int32(1),
			chipsPerHost:  int64(8),
		},
		"getNumTPUHostsFromTopology with v5litepod-16 4x4 topology": {
			// v5e - 4x4 and 4 chips per VM, should return 4 TPU VMs
			topology:      "4x4",
			expectedHosts: int32(4),
			chipsPerHost:  int64(4),
		},
	}

	// validate that getNumTPUHostsFromTopology returns the expected # TPU VM Hosts for varying TPU podslice types
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			vms, err := getNumTPUHostsFromTopology("test-cluster", "test-group", "test-namespace", tc.topology, tc.chipsPerHost)
			if err == nil {
				assert.Equal(t, tc.expectedHosts, vms)
			}
			if tc.topology == "" {
				assert.Equal(t, tc.expectedError, err)
			}
		})
	}
}

func Test_GetNumTPUChipsRequested(t *testing.T) {
	tests := map[string]struct {
		testPod            *corev1.Pod
		expectedTPULimit   map[corev1.ResourceName]resource.Quantity
		expectedTPURequest map[corev1.ResourceName]resource.Quantity
		expectedNumChips   int64
	}{
		"getNumTPUChipsRequested no TPUs requested": {
			// doesn't request TPUs - returns 0
			testPod:            getTestCPUWorker("test-cluster", "test-group", "test-namespace"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("0")},
			expectedTPURequest: map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("0")},
			expectedNumChips:   int64(0),
		},
		"getNumTPUChipsRequested only TPU limit resource set": {
			// includes TPU limits but omits request - defaults to limit value
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("4")},
			expectedTPURequest: nil,
			expectedNumChips:   int64(4),
		},
		"getNumTPUChipsRequested with TPU Request > TPU Limit": {
			// TPU Limit = maximum number of TPU chips requested
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("4")},
			expectedTPURequest: map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("8")},
			expectedNumChips:   int64(4),
		},
		"getNumTPUChipsRequested with v4 TPU request": {
			// v4 - always 4 chips per VM
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x2", "4"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("4")},
			expectedTPURequest: map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("4")},
			expectedNumChips:   int64(4),
		},
		"getNumTPUChipsRequested with v5e ct5lp-hightpu-1t TPU request": {
			// v5e - 1x1 and 1 chip per VM
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v5-lite-podslice", "1x1", "1"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("1")},
			expectedTPURequest: map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("1")},
			expectedNumChips:   int64(1),
		},
		"getNumTPUChipsRequested with v5e ct5lp-hightpu-8t TPU request": {
			// v5e - 2x4 and 8 chips per VM
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v5-lite-podslice", "2x4", "8"),
			expectedTPULimit:   map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("8")},
			expectedTPURequest: map[corev1.ResourceName]resource.Quantity{"google.com/tpu": resource.MustParse("8")},
			expectedNumChips:   int64(8),
		},
	}

	// validate that getNumTPUChipsRequested correctly returns the number of TPU chips requested per Pod container
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			container := tc.testPod.Spec.Containers[0]
			container.Resources.Limits = tc.expectedTPULimit
			container.Resources.Requests = tc.expectedTPURequest
			chipsPerHost := getNumTPUChipsRequested(container)
			assert.Equal(t, tc.expectedNumChips, chipsPerHost)
		})
	}
}

func Test_ExtractPod(t *testing.T) {
	tests := map[string]struct {
		testPod       *corev1.Pod
		expectedKind  string
		expectedError error
	}{
		"extractPod with wrong admissionRequest Kind": {
			// should return an error since Kind != Pod
			testPod:       getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			expectedKind:  "RayCluster",
			expectedError: errors.New("Expected Pod but got RayCluster"),
		},
		"extractPod with admissionRequest Kind == Pod": {
			// should successfully unmarshal the Pod object
			testPod:      getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			expectedKind: "Pod",
		},
	}

	// validate that extractPod correctly unmarshals admissionReview object
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// set up admissionReview object
			admissionReview := getTestAdmissionReview(tc.expectedKind, "CREATE")
			jsonPod, _ := json.Marshal(tc.testPod)
			admissionReview.Request.Object.Raw = jsonPod
			admissionReview.Request.Object.Object = tc.testPod

			// set Request Kind
			admissionReview.Request.Kind.Kind = tc.expectedKind

			actualPod, err := extractPod(admissionReview)
			if err != nil {
				assert.Equal(t, tc.expectedError, err)
			} else {
				// jsons don't match exactly after marshal -> unmarshal so just check fields
				assert.Equal(t, tc.testPod.Name, actualPod.Name)
				assert.Equal(t, tc.testPod.Namespace, actualPod.Namespace)
			}
		})
	}
}

func Test_ExtractRayCluster(t *testing.T) {
	tests := map[string]struct {
		testRayCluster *rayv1.RayCluster
		expectedKind   string
		expectedError  error
	}{
		"extractRayCluster with wrong admissionRequest Kind": {
			// should return an error since Kind != RayCluster
			testRayCluster: getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 1, "0", "", "", false),
			expectedKind:   "Pod",
			expectedError:  errors.New("Expected RayCluster but got Pod"),
		},
		"extractRayCluster with admissionRequest Kind == RayCluster": {
			// should successfully unmarshal the RayCluster object
			testRayCluster: getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 1, "0", "", "", false),
			expectedKind:   "RayCluster",
		},
	}

	// validate that extractRayCluster correctly unmarshals admissionReview object
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// set up admissionReview object
			admissionReview := getTestAdmissionReview(tc.expectedKind, "CREATE")
			jsonRayCluster, _ := json.Marshal(tc.testRayCluster)
			admissionReview.Request.Object.Raw = jsonRayCluster
			admissionReview.Request.Object.Object = tc.testRayCluster

			// set Request Kind
			admissionReview.Request.Kind.Kind = tc.expectedKind

			actualRayCluster, err := extractRayCluster(admissionReview)
			if err != nil {
				assert.Equal(t, tc.expectedError, err)
			} else {
				// jsons don't match exactly after marshal -> unmarshal so just check fields
				assert.Equal(t, tc.testRayCluster.Name, actualRayCluster.Name)
				// assert.Equal(t, tc.testRayCluster.Spec.WorkerGroupSpecs, actualRayCluster.Spec.WorkerGroupSpecs)
			}
		})
	}
}

func Test_GenDNSHostnames(t *testing.T) {
	tests := map[string]struct {
		clusterName       string
		replicaIndex      int
		numOfHosts        int32
		expectedHostnames string
		expectedError     error
	}{
		"genDNSHostnames with NumOfHosts == 0": {
			// a workergroup can't have NumOfHosts set to 0 so this should error out
			clusterName:   "test-cluster",
			replicaIndex:  0,
			numOfHosts:    int32(0),
			expectedError: errors.New("workerGroupSpec NumOfHosts not set"),
		},
		"genDNSHostnames with NumOfHosts == 1": {
			// Single-host worker group, should return a single DNS hostname. This function will
			// never be called for single-host groups, but we don't necessarily want it to error if it does.
			clusterName:       "test-cluster",
			replicaIndex:      0,
			numOfHosts:        int32(1),
			expectedHostnames: fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
		},
		"genDNSHostnames with NumOfHosts > 1": {
			// multi-host worker group, should return a string list of DNS hostnames for the given replica
			clusterName:  "test-cluster",
			replicaIndex: 1,
			numOfHosts:   int32(4),
			expectedHostnames: strings.Join([]string{fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
		},
		"genDNSHostnames with long RayCluster name": {
			// Multi-host worker group in a RayCluster with a name that will be truncated
			clusterName:  "extremely-long-raycluster-name-to-be-truncated",
			replicaIndex: 1,
			numOfHosts:   int32(2),
			expectedHostnames: strings.Join([]string{fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "mely-long-raycluster-name-to-be-truncated", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "mely-long-raycluster-name-to-be-truncated", utils.HeadlessServiceSuffix),
			}, ","),
		},
	}

	// validate that genDNSHostnames correctly returns a string list of DNS addressable hostnames
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			hostnames, err := genDNSHostnames(tc.numOfHosts, "test-group", tc.clusterName, "test-namespace", tc.replicaIndex)
			if err != nil {
				assert.Equal(t, tc.expectedError, err)
			} else {
				assert.Equal(t, tc.expectedHostnames, hostnames)
			}
		})
	}
}

func Test_InjectHostnames(t *testing.T) {
	tests := map[string]struct {
		clusterName       string
		groupName         string
		expectedSubdomain string
		expectedHostnames string
	}{
		"injectHostnames for multi-host worker group": {
			// Should create a patch to set the subdomain and TPU_WORKER_HOSTNAMES for all hosts.
			// This function is only called for multi-host TPU worker groups.
			clusterName:       "test-cluster",
			groupName:         "test-group-name",
			expectedSubdomain: fmt.Sprintf("%s-%s", "test-cluster", utils.HeadlessServiceSuffix),
			expectedHostnames: strings.Join([]string{fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
		},
		"injectHostnames for multi-host worker group with truncated service name": {
			// Should create a patch to set the subdomain and TPU_WORKER_HOSTNAMES for all hosts, with the
			// correct subdomain truncated to match the created service name.
			clusterName:       "really-really-extremely-long-test-raycluster-name",
			groupName:         "test-group-name",
			expectedSubdomain: fmt.Sprintf("%s-%s", "eally-extremely-long-test-raycluster-name", utils.HeadlessServiceSuffix),
			expectedHostnames: strings.Join([]string{fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "eally-extremely-long-test-raycluster-name", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "eally-extremely-long-test-raycluster-name", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 2, "eally-extremely-long-test-raycluster-name", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 3, "eally-extremely-long-test-raycluster-name", utils.HeadlessServiceSuffix),
			}, ","),
		},
	}

	// check that a valid subdomain and TPU_WORKER_HOSTNAMES are injected into the Pod
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			testPod := getTestTPUWorker(tc.clusterName, tc.groupName, "test-namespace", "tpu-v4-podslice", "2x2x2", "4")
			expectedEnv := []corev1.EnvVar{corev1.EnvVar{Name: "TPU_WORKER_HOSTNAMES", Value: tc.expectedHostnames}}
			patches := []patch{}
			injectHostnames(tc.clusterName, tc.expectedHostnames, "/spec/containers/0/env", testPod.Spec.Containers[0], &patches)
			// check subdomain patch
			assert.Equal(t, "/spec/subdomain", patches[0]["path"])
			assert.Equal(t, tc.expectedSubdomain, patches[0]["value"])
			// check hostnames patch
			assert.Equal(t, "/spec/containers/0/env", patches[1]["path"])
			assert.Equal(t, expectedEnv, patches[1]["value"])
		})
	}
}

func Test_InjectReplicaLabel(t *testing.T) {
	tests := map[string]struct {
		replicaIndex         int
		groupName            string
		expectedReplicaLabel string
	}{
		"injectReplicaLabel with replicaIndex 0": {
			// should create a patch to set the replicaIndex label with {$WORKER_GROUP_NAME-$REPLICA_INDEX}
			replicaIndex:         0,
			groupName:            "test-group-name",
			expectedReplicaLabel: "test-group-name-0",
		},
	}

	// validate that injectReplicaLabel creates a patch with a valid replicaIndex label
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			expectedPatches := []patch{}
			injectReplicaLabel("test-cluster", "test-namespace", tc.replicaIndex, tc.groupName, &expectedPatches)
			assert.Equal(t, "/metadata/labels/replicaIndex", expectedPatches[0]["path"])
			assert.Equal(t, tc.expectedReplicaLabel, expectedPatches[0]["value"])
		})
	}
}

func Test_InjectAffinity(t *testing.T) {
	tests := map[string]struct {
		testPod              *corev1.Pod
		replicaIndex         int
		groupName            string
		expectedReplicaLabel string
		expectedClusterLabel string
	}{
		"injectAffinity with replicaIndex and cluster labels": {
			testPod:              getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			replicaIndex:         0,
			groupName:            "test-group-name",
			expectedReplicaLabel: "test-group-name-0",
			expectedClusterLabel: "test-cluster",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var patches []patch

			injectAffinity(tc.testPod, tc.replicaIndex, tc.groupName, &patches)

			assert.Len(t, patches, 1)
			assert.Equal(t, "/spec/affinity", patches[0]["path"])

			affinity := patches[0]["value"].(corev1.Affinity)

			// Validate PodAffinity is injected with expected label selectors
			podAffinityTerms := affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			podMatchExprs := map[string]metav1.LabelSelectorRequirement{}
			for _, expr := range podAffinityTerms[0].LabelSelector.MatchExpressions {
				podMatchExprs[expr.Key] = expr
			}
			assert.Equal(t, metav1.LabelSelectorOpIn, podMatchExprs["replicaIndex"].Operator)
			assert.Equal(t, []string{tc.expectedReplicaLabel}, podMatchExprs["replicaIndex"].Values)
			assert.Equal(t, metav1.LabelSelectorOpIn, podMatchExprs["ray.io/cluster"].Operator)
			assert.Equal(t, []string{tc.expectedClusterLabel}, podMatchExprs["ray.io/cluster"].Values)

			// Validate PodAntiAffinity is injected with expected label selectors
			podAntiAffinityTerms := affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			// Check anti-affinity for Pods of same cluster and different replicaIndex
			term1MatchExprs := map[string]metav1.LabelSelectorRequirement{}
			for _, expr := range podAntiAffinityTerms[0].LabelSelector.MatchExpressions {
				term1MatchExprs[expr.Key] = expr
			}
			assert.Equal(t, metav1.LabelSelectorOpNotIn, term1MatchExprs["replicaIndex"].Operator)
			assert.Equal(t, []string{tc.expectedReplicaLabel}, term1MatchExprs["replicaIndex"].Values)
			assert.Equal(t, metav1.LabelSelectorOpIn, term1MatchExprs["ray.io/cluster"].Operator)
			assert.Equal(t, []string{tc.expectedClusterLabel}, term1MatchExprs["ray.io/cluster"].Values)

			// Check anti-affinity for Pods of different cluster when replicaIndex exists
			term2MatchExprs := map[string]metav1.LabelSelectorRequirement{}
			for _, expr := range podAntiAffinityTerms[1].LabelSelector.MatchExpressions {
				term2MatchExprs[expr.Key] = expr
			}
			assert.Equal(t, metav1.LabelSelectorOpExists, term2MatchExprs["replicaIndex"].Operator)
			assert.Equal(t, metav1.LabelSelectorOpNotIn, term2MatchExprs["ray.io/cluster"].Operator)
			assert.Equal(t, []string{tc.expectedClusterLabel}, term2MatchExprs["ray.io/cluster"].Values)
			assert.NotNil(t, podAntiAffinityTerms[1].NamespaceSelector)
		})
	}
}

func Test_CheckWorkersMatchTopology(t *testing.T) {
	tests := map[string]struct {
		expectedNumOfHosts  int32
		expectedAccelerator string
		expectedTopology    string
		expectedTPUChips    string
		missingContainers   bool
		expectedError       error
		workersMatch        bool
	}{
		"checkWorkersMatchTopology NumOfHosts == 0": {
			// returns false and an error
			expectedNumOfHosts: 0,
			expectedTPUChips:   "0",
			expectedError:      errors.New("workerGroupSpec NumOfHosts not set"),
			workersMatch:       false,
		},
		"checkWorkersMatchTopology WorkerGroup containers missing": {
			// containers == nil, returns false and an error
			missingContainers:  true,
			expectedNumOfHosts: 1,
			expectedTPUChips:   "0",
			expectedError:      errors.New("Container path not specified"),
			workersMatch:       false,
		},
		"checkWorkersMatchTopology missing topology nodeSelector": {
			// topology not set, returns false and an error
			expectedNumOfHosts: 1,
			expectedTopology:   "",
			expectedTPUChips:   "4",
			expectedError:      errors.New("TPU topology not specified"),
			workersMatch:       false,
		},
		"checkWorkersMatchTopology NumOfHosts not equal to specified topology": {
			// topology does not match NumOfHosts, returns false
			expectedNumOfHosts:  1,
			expectedAccelerator: "tpu-v4-podslice",
			expectedTopology:    "2x2x2",
			expectedTPUChips:    "4",
			workersMatch:        false,
		},
		"checkWorkersMatchTopology v4 single-host NumOfHosts equal to specified topology": {
			// topology matches NumOfHosts, returns true
			expectedNumOfHosts:  1,
			expectedAccelerator: "tpu-v4-podslice",
			expectedTopology:    "2x2x1",
			expectedTPUChips:    "4",
			workersMatch:        true,
		},
		"checkWorkersMatchTopology v4 multi-host NumOfHosts equal to specified topology": {
			// topology matches NumOfHosts, returns true
			expectedNumOfHosts:  4,
			expectedAccelerator: "tpu-v4-podslice",
			expectedTopology:    "2x2x4",
			expectedTPUChips:    "4",
			workersMatch:        true,
		},
		"checkWorkersMatchTopology v5 single-host NumOfHosts equal to specified topology": {
			// topology matches NumOfHosts, returns true
			expectedNumOfHosts:  1,
			expectedAccelerator: "tpu-v5-lite-device",
			expectedTopology:    "2x4",
			expectedTPUChips:    "8",
			workersMatch:        true,
		},
		"checkWorkersMatchTopology v5 multi-host NumOfHosts equal to specified topology": {
			// topology matches NumOfHosts, returns true
			expectedNumOfHosts:  2,
			expectedAccelerator: "tpu-v5-lite-device",
			expectedTopology:    "2x4",
			expectedTPUChips:    "4",
			workersMatch:        true,
		},
	}

	// validate checkWorkersMatchTopology returns true only when NumOfHosts == # TPU VMs specified by topology
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// set up worker group object for test
			workerGroupSpec := getTestTPUWorkerGroup("test-group", tc.expectedNumOfHosts, 1, tc.expectedAccelerator, tc.expectedTopology, tc.expectedTPUChips)
			if tc.missingContainers {
				workerGroupSpec.Template.Spec.Containers = nil
			}

			workersMatchTopology, err := checkWorkersMatchTopology("test-cluster", "test-namespace", *workerGroupSpec)

			if tc.expectedNumOfHosts == 0 || tc.missingContainers == true || tc.expectedTopology == "" {
				assert.Equal(t, tc.expectedError, err)
			}
			assert.Equal(t, tc.workersMatch, workersMatchTopology)
		})
	}
}

func Test_ValidateRayCluster(t *testing.T) {
	tests := map[string]struct {
		rayCluster          *rayv1.RayCluster
		missingWorkerGroups bool
		expectedResponse    *admissionv1.AdmissionResponse
		expectedAllowed     bool
		expectedResult      *metav1.Status
	}{
		"validateRayCluster no workerGroupSpecs": {
			// doesn't create any workergroups, pass-through
			rayCluster:          getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 1, "0", "", "", false),
			missingWorkerGroups: false,
			expectedAllowed:     true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster no TPUs requested": {
			// doesn't request TPUs, pass-through
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 1, "0", "", "", false),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster worker group spec not compatible with gke-tpu-topology": {
			// request TPUs, workers don't match topology, return false
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(2), 1, "4", "tpu-v4-podslice", "2x2x1", false),
			expectedAllowed: false,
			expectedResult: &metav1.Status{
				Status:  "Failure",
				Message: "Number of workers in worker group not equal to specified topology",
			},
		},
		"validateRayCluster RayCluster with single-slice, single-host TPU worker group": {
			// request TPUs, workers match topology, return true
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 1, "4", "tpu-v4-podslice", "2x2x1", false),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster RayCluster with single-slice, multi-host TPU worker group": {
			// request TPUs, workers match topology, return true
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(4), 1, "4", "tpu-v4-podslice", "2x2x4", false),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster RayCluster with multi-slice, single-host TPU worker group": {
			// request TPUs, workers match topology, return true
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(1), 4, "4", "tpu-v4-podslice", "2x2x1", false),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster RayCluster with multi-slice, multi-host TPU worker group": {
			// request TPUs, workers match topology, return true
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(4), 4, "4", "tpu-v4-podslice", "2x2x4", false),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
		"validateRayCluster RayCluster with TPU and GPU worker groups": {
			// request TPUs, ignored GPU group, workers match topology, return true
			rayCluster:      getTestRayCluster("test-cluster", "test-group", "test-namespace", int32(4), 4, "4", "tpu-v4-podslice", "2x2x4", true),
			expectedAllowed: true,
			expectedResult: &metav1.Status{
				Status:  "Success",
				Message: "",
			},
		},
	}

	// check validateRayCluster
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// set up admissionReview object
			admissionReview := getTestAdmissionReview("RayCluster", "CREATE")
			// set RayCluster worker group values
			if tc.missingWorkerGroups {
				tc.rayCluster.Spec.WorkerGroupSpecs = nil
			}
			jsonRayCluster, _ := json.Marshal(tc.rayCluster)
			admissionReview.Request.Object.Raw = jsonRayCluster
			admissionReview.Request.Object.Object = tc.rayCluster

			// test validateRayCluster admissionResponse output
			admissionResponse, _ := validateRayCluster(admissionReview)
			if admissionResponse != nil {
				assert.Equal(t, tc.expectedAllowed, admissionResponse.Allowed)
				assert.Equal(t, tc.expectedResult.Status, admissionResponse.Result.Status)
				assert.Equal(t, tc.expectedResult.Message, admissionResponse.Result.Message)
			}
		})
	}
}

func Test_GetEnvironmentVariable(t *testing.T) {
	// initialize test container object
	testTPUWorker := getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4")
	podContainer := testTPUWorker.Spec.Containers[0].DeepCopy()
	workerID := corev1.EnvVar{
		Name:  "TPU_WORKER_ID",
		Value: "0",
	}
	workerName := corev1.EnvVar{
		Name:  "TPU_NAME",
		Value: fmt.Sprintf("%s-%d", "test-group", 0),
	}
	workerHostnames := corev1.EnvVar{
		Name:  "TPU_WORKER_HOSTNAMES",
		Value: fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
	}
	workerDevicePluginAddr := corev1.EnvVar{
		Name:  "TPU_DEVICE_PLUGIN_ADDR",
		Value: "$(TPU_DEVICE_PLUGIN_HOST_IP):2112",
	}
	podContainer.Env = []corev1.EnvVar{workerID, workerName, workerHostnames, workerDevicePluginAddr}

	tests := map[string]struct {
		variableName       string
		container          *corev1.Container
		expectedValue      string
		expectedValueExist bool
	}{
		"getEnvironmentVariable TPU_WORKER_ID": {
			// returns TPU_WORKER_ID env var value
			variableName:  "TPU_WORKER_ID",
			container:     podContainer,
			expectedValue: "0",
		},
		"getEnvironmentVariable TPU_NAME": {
			// returns TPU_NAME env var value
			variableName:  "TPU_NAME",
			container:     podContainer,
			expectedValue: fmt.Sprintf("%s-%d", "test-group", 0),
		},
		"getEnvironmentVariable TPU_WORKER_HOSTNAMES": {
			// returns TPU_WORKER_HOSTNAMES env var value
			variableName:  "TPU_WORKER_HOSTNAMES",
			container:     podContainer,
			expectedValue: fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
		},
		"getEnvironmentVariable TPU_DEVICE_PLUGIN_HOST_IP": {
			// returns TPU_DEVICE_PLUGIN_HOST_IP env var check
			variableName:       "TPU_DEVICE_PLUGIN_HOST_IP",
			container:          podContainer,
			expectedValue:      "",
			expectedValueExist: false,
		},
		"getEnvironmentVariable TPU_DEVICE_PLUGIN_ADDR": {
			variableName:  "TPU_DEVICE_PLUGIN_ADDR",
			container:     podContainer,
			expectedValue: "$(TPU_DEVICE_PLUGIN_HOST_IP):2112",
		},
	}

	// validate getEnvironmentVariable returns correct env var value from Pod container
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			varValue, exists := getEnvironmentVariable(tc.variableName, *tc.container)
			assert.Equal(t, tc.expectedValue, varValue)
			if tc.expectedValueExist {
				assert.True(t, exists)
			}
		})
	}
}

func Test_GetSliceToWorkerIDs(t *testing.T) {
	testCPUWorker := getTestCPUWorker("test-cluster", "test-group", "test-namespace")
	testTPUWorker := getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x2", "4")
	expectedIds := make([]int, 64)
	for i := 0; i < 64; i++ {
		expectedIds[i] = i
	}

	tests := map[string]struct {
		numOfHosts               int32
		numReplicas              int
		podsInGroup              []*corev1.Pod
		expectedSliceToWorkerIDs map[slice][]int
	}{
		"getSliceToWorkerIDs with nil Pod list": {
			// this can occur when no Pods with the Ray group name have been cached
			// should return an empty mapping
			podsInGroup:              nil,
			expectedSliceToWorkerIDs: make(map[slice][]int),
		},
		"getSliceToWorkerIDs for with CPU pod list": {
			// sliceToWorkerIDs should return an empty mapping
			numOfHosts:               int32(1),
			numReplicas:              4,
			podsInGroup:              getTestPods(testCPUWorker, "test-cluster", "test-namespace", 4),
			expectedSliceToWorkerIDs: make(map[slice][]int),
		},
		"getSliceToWorkerIDs for with TPU pod list": {
			// sliceToWorkerIDs should be populated with TPU worker IDs
			numOfHosts:  int32(64),
			numReplicas: 2,
			podsInGroup: getTestInterceptedTPUPods(testTPUWorker, 128, 2, 64),
			expectedSliceToWorkerIDs: map[slice][]int{
				slice{"test-cluster", "test-group", "test-namespace", 0, int32(64)}: expectedIds,
				slice{"test-cluster", "test-group", "test-namespace", 1, int32(64)}: expectedIds,
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			podLister := setupInformer(tc.podsInGroup...)
			tpuWebhook := NewTPUWebhookServer(podLister)
			sliceToWorkerIDs, err := tpuWebhook.getSliceToWorkerIDs("test-cluster", "test-group", "test-namespace", tc.numOfHosts)

			// sliceToWorkerIDs should be populated with slices and unique TPU_WORKER_IDs for each Pod
			assert.Equal(t, err, nil)
			for slice, workerIDs := range sliceToWorkerIDs {
				assert.Contains(t, tc.expectedSliceToWorkerIDs, slice)
				assert.Equal(t, len(tc.expectedSliceToWorkerIDs[slice]), len(workerIDs))
				sort.Ints(workerIDs)
				for index, value := range workerIDs {
					assert.Equal(t, tc.expectedSliceToWorkerIDs[slice][index], value)
				}
			}
		})
	}
}

func Test_IsLastAdmittedPod(t *testing.T) {
	tests := map[string]struct {
		testPod        *corev1.Pod
		testWorkerID   string
		testReplicaID  string
		lastAdmitted   string
		isLastAdmitted bool
		expectedError  error
	}{
		"isLastAdmittedPod Pod missing RayCluster label": {
			// missing Ray cluster label - returns error
			testPod:        getTestCPUWorker("", "test-group", "test-namespace"),
			expectedError:  errors.New("Ray Pod created by KubeRay missing RayCluster label"),
			isLastAdmitted: false,
		},
		"isLastAdmittedPod Pod does not request TPUs": {
			// pod is not a TPU pod, should return false
			testPod:        getTestCPUWorker("test-cluster", "test-group", "test-namespace"),
			isLastAdmitted: false,
		},
		"isLastAdmittedPod TPU Pod does not match lastAdmitted": {
			// TPU pod does not match lastAdmitted, return false
			testPod:        getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v6e-slice", "4x4", "4"),
			testWorkerID:   "4",
			testReplicaID:  "test-group-0",
			lastAdmitted:   "test-namespace-test-cluster-test-group-0-3",
			isLastAdmitted: false,
		},
		"isLastAdmittedPod TPU Pod matches lastAdmitted": {
			// TPU pod matches lastAdmitted, return true
			testPod:        getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v6e-slice", "4x4", "4"),
			testWorkerID:   "0",
			testReplicaID:  "test-group-0",
			lastAdmitted:   "test-namespace-test-cluster-test-group-0-0",
			isLastAdmitted: true,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			// set TPU_WORKER_ID for testPod
			if containerRequestingTPUs(tc.testPod.Spec.Containers...) {
				if tc.testReplicaID != "" {
					tc.testPod.Labels["replicaIndex"] = tc.testReplicaID
					tc.testPod.Spec.Containers[0].Env = []corev1.EnvVar{
						{
							Name:  "TPU_WORKER_ID",
							Value: tc.testWorkerID,
						},
					}
				}
			}
			// set up TPUWebhookServer
			testPodLister := setupInformer(tc.testPod)
			tpuWebhookServer := NewTPUWebhookServer(testPodLister)
			tpuWebhookServer.lastAdmitted = tc.lastAdmitted

			isLastAdmitted, err := tpuWebhookServer.isLastAdmittedPod(tc.testPod)
			if err != nil {
				assert.Equal(t, tc.expectedError, err)
			} else {
				assert.Equal(t, tc.isLastAdmitted, isLastAdmitted)
			}
		})
	}
}

func Test_MutatePod(t *testing.T) {
	tests := map[string]struct {
		testPod                      *corev1.Pod
		numOfHosts                   int
		existingPods                 int
		existingReplicas             int
		missingContainers            bool
		expectedWorkerID             string
		expectedReplicaID            int
		expectedWorkerName           string
		expectedHostnames            string
		expectedReplicaLabel         string
		expectedError                error
		expectedDevicePluginHostAddr map[string]interface{}
		expectedDevicePluginAddr     string
		checkMultiSlice              bool
		useKubeRayLabels             bool
	}{
		"mutatePod for CPU pod": {
			// no TPU requested - pass through.
			testPod:       getTestCPUWorker("test-cluster", "test-group", "test-namespace"),
			expectedError: nil,
		},
		"mutatePod missing cluster label": {
			// missing Ray cluster label - returns error
			testPod:       getTestTPUWorker("", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			expectedError: errors.New("Ray Pod created by KubeRay missing RayCluster label"),
		},
		"mutatePod missing container": {
			// missing containers - returns error
			testPod:           getTestCPUWorker("test-cluster", "test-group", "test-namespace"),
			missingContainers: true,
			expectedError:     errors.New("Container path not specified"),
		},
		"mutatePod missing gke-tpu-topology nodeSelector": {
			// requests TPUs, topology not specified - returns error
			testPod:           getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "", "4"),
			missingContainers: false,
			expectedError:     errors.New("Ray Pod created by KubeRay missing TPU topology nodeSelector"),
		},
		"mutatePod in single-host TPU worker group": {
			// requests TPUs, single-host - injects TPU_WORKER_ID, TPU_NAME and replicaIndex label
			testPod:                      getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x1", "4"),
			numOfHosts:                   1,
			existingPods:                 0,
			existingReplicas:             0,
			expectedWorkerID:             "0",
			expectedReplicaID:            0,
			expectedWorkerName:           fmt.Sprintf("%s-%d", "test-group", 0),
			expectedReplicaLabel:         fmt.Sprintf("%s-%d", "test-group", 0),
			expectedDevicePluginHostAddr: map[string]interface{}{"fieldRef": map[string]interface{}{"fieldPath": "status.hostIP"}},
			expectedDevicePluginAddr:     "$(TPU_DEVICE_PLUGIN_HOST_IP):2112",
		},
		"mutatePod first Pod in multi-host TPU worker group": {
			// requests TPUs, multi-host - injects hostname, subdomain, TPU_WORKER_ID, TPU_NAME,
			// TPU_HOSTNAMES, a podAffinity field, and the replicaIndex label
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x4", "4"),
			numOfHosts:         4,
			existingPods:       0,
			existingReplicas:   0,
			expectedWorkerID:   "0",
			expectedReplicaID:  0,
			expectedWorkerName: fmt.Sprintf("%s-%d", "test-group", 0),
			expectedHostnames: strings.Join([]string{
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
			expectedReplicaLabel: fmt.Sprintf("%s-%d", "test-group", 0),
		},
		"mutatePod subsequent Pod in multi-host TPU worker group": {
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x4", "4"),
			numOfHosts:         4,
			existingPods:       3,
			existingReplicas:   1,
			expectedWorkerID:   "3",
			expectedReplicaID:  0,
			expectedWorkerName: fmt.Sprintf("%s-%d", "test-group", 0),
			expectedHostnames: strings.Join([]string{
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
			expectedReplicaLabel: fmt.Sprintf("%s-%d", "test-group", 0),
		},
		"mutatePod first multi-host Pod in subsequent multi-slice TPU worker group": {
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x4", "4"),
			numOfHosts:         4,
			existingPods:       4,
			existingReplicas:   1,
			expectedWorkerID:   "0",
			expectedReplicaID:  1,
			expectedWorkerName: fmt.Sprintf("%s-%d", "test-group", 1),
			expectedHostnames: strings.Join([]string{
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
			expectedReplicaLabel: fmt.Sprintf("%s-%d", "test-group", 1),
		},
		"mutatePod subsequent multi-host Pod in subsequent multi-slice TPU worker group": {
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x4", "4"),
			numOfHosts:         4,
			existingPods:       5,
			existingReplicas:   1,
			expectedWorkerID:   "1",
			expectedReplicaID:  1,
			expectedWorkerName: fmt.Sprintf("%s-%d", "test-group", 1),
			expectedHostnames: strings.Join([]string{
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 1, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
			expectedReplicaLabel: fmt.Sprintf("%s-%d", "test-group", 1),
		},
		"mutatePod Multi-Slice TPU worker group": {
			testPod:            getTestTPUWorker("test-cluster", "test-group", "test-namespace", "tpu-v4-podslice", "2x2x4", "4"),
			numOfHosts:         4,
			existingPods:       0,
			existingReplicas:   0,
			expectedWorkerID:   "0",
			expectedReplicaID:  0,
			expectedWorkerName: fmt.Sprintf("%s-%d", "test-group", 0),
			expectedHostnames: strings.Join([]string{
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 0, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 1, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 2, "test-cluster", utils.HeadlessServiceSuffix),
				fmt.Sprintf("%s-%d-%d.%s-%s", "test-group", 0, 3, "test-cluster", utils.HeadlessServiceSuffix),
			}, ","),
			expectedReplicaLabel: fmt.Sprintf("%s-%d", "test-group", 0),
			checkMultiSlice:      true,
			useKubeRayLabels:     true,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			inputPod := tc.testPod.DeepCopy()

			if tc.missingContainers {
				inputPod.Spec.Containers = nil
			} else if tc.checkMultiSlice {
				// Inject MEGASCALE_NUM_SLICES so that the webhook injects dynamically
				// set multi-slice vars to Pods. MEGASCALE_NUM_SLICES is expected to be set
				// by the user or in the Ray application code.
				inputPod.Spec.Containers[0].Env = append(inputPod.Spec.Containers[0].Env, corev1.EnvVar{
					Name:  "MEGASCALE_NUM_SLICES",
					Value: "2",
				})
			}

			if tc.useKubeRayLabels {
				if inputPod.Labels == nil {
					inputPod.Labels = make(map[string]string)
				}
				inputPod.Labels[utils.RayWorkerReplicaIndexKey] = fmt.Sprint(tc.expectedReplicaID)
				inputPod.Labels[utils.RayHostIndexKey] = tc.expectedWorkerID
			}

			// set up admissionReview object
			admissionReview := getTestAdmissionReview("Pod", "CREATE")
			jsonPod, _ := json.Marshal(inputPod)
			admissionReview.Request.Object.Raw = jsonPod
			admissionReview.Request.Object.Object = inputPod

			// generate Pod list for the lister (simulating EXISTING pods, not the one being created)
			testTPUPods := getTestInterceptedTPUPods(inputPod, tc.existingPods, tc.existingReplicas, tc.numOfHosts)
			testPodLister := setupInformer(testTPUPods...)

			// set up TPUWebhookServer
			tpuWebhookServer := NewTPUWebhookServer(testPodLister)
			admissionResponse, err := tpuWebhookServer.mutatePod(admissionReview)

			if tc.expectedError != nil {
				assert.Equal(t, tc.expectedError, err)
				return
			}

			if !containerRequestingTPUs(inputPod.Spec.Containers...) {
				assert.Empty(t, admissionResponse.Patch, "Expected no patches for non-TPU worker")
				return
			}

			var patches []patch
			json.Unmarshal(admissionResponse.Patch, &patches)

			// Helper: Find patch value by exact path
			findPatchValue := func(path string) interface{} {
				for _, p := range patches {
					if p["path"] == path {
						return p["value"]
					}
				}
				return nil
			}

			// Helper: Find Env Var map by name
			findEnvVarPatch := func(name string) map[string]interface{} {
				for _, p := range patches {
					if valList, ok := p["value"].([]interface{}); ok {
						for _, v := range valList {
							if vMap, ok := v.(map[string]interface{}); ok {
								if vMap["name"] == name {
									return vMap
								}
							}
						}
					}
					if valMap, ok := p["value"].(map[string]interface{}); ok {
						if valMap["name"] == name {
							return valMap
						}
					}
				}
				return nil
			}

			// Validate all expected patches exist.
			// Check replicaIndex Label
			if tc.useKubeRayLabels {
				// In this case, we shouldn't inject a label because it's handled by KubeRay.
				assert.Nil(t, findPatchValue("/metadata/labels/replicaIndex"), "Legacy replicaIndex label should NOT be patched.")
			} else {
				assert.Equal(t, tc.expectedReplicaLabel, findPatchValue("/metadata/labels/replicaIndex"))
			}

			// 1. Check Replica Label
			if tc.useKubeRayLabels {
				assert.Nil(t, findPatchValue("/metadata/labels/replicaIndex"), "Legacy replicaIndex label should NOT be patched in Fast Path")
			} else {
				assert.Equal(t, tc.expectedReplicaLabel, findPatchValue("/metadata/labels/replicaIndex"))
			}

			// 2. Check TPU_WORKER_ID
			workerIDMap := findEnvVarPatch("TPU_WORKER_ID")
			if assert.NotNil(t, workerIDMap, "TPU_WORKER_ID patch missing") {
				assert.Equal(t, tc.expectedWorkerID, workerIDMap["value"])
			}

			// 3. Check TPU_NAME
			workerNameMap := findEnvVarPatch("TPU_NAME")
			if assert.NotNil(t, workerNameMap, "TPU_NAME patch missing") {
				assert.Equal(t, tc.expectedWorkerName, workerNameMap["value"])
			}

			// 4. Check Device Plugin Host IP
			pluginHostMap := findEnvVarPatch("TPU_DEVICE_PLUGIN_HOST_IP")
			assert.NotNil(t, pluginHostMap, "TPU_DEVICE_PLUGIN_HOST_IP missing")

			// 5. Check Device Plugin Addr
			pluginAddrMap := findEnvVarPatch("TPU_DEVICE_PLUGIN_ADDR")
			if assert.NotNil(t, pluginAddrMap, "TPU_DEVICE_PLUGIN_ADDR missing") {
				assert.Equal(t, "$(TPU_DEVICE_PLUGIN_HOST_IP):2112", pluginAddrMap["value"])
			}

			if tc.numOfHosts > 1 {
				// 6. Check Hostname
				assert.Equal(t, fmt.Sprintf("%s-%s", tc.expectedReplicaLabel, tc.expectedWorkerID), findPatchValue("/spec/hostname"))

				// 7. Check Subdomain
				assert.Equal(t, fmt.Sprintf("%s-%s", "test-cluster", utils.HeadlessServiceSuffix), findPatchValue("/spec/subdomain"))

				// 8. Check TPU_WORKER_HOSTNAMES
				hostnamesMap := findEnvVarPatch("TPU_WORKER_HOSTNAMES")
				if assert.NotNil(t, hostnamesMap, "TPU_WORKER_HOSTNAMES patch missing") {
					assert.Equal(t, tc.expectedHostnames, hostnamesMap["value"])
				}

				// 9. Check Affinity
				affinity := findPatchValue("/spec/affinity")
				assert.NotNil(t, affinity, "Affinity patch missing")
			}

			// 10. Check Multi-Slice / Megascale
			if tc.checkMultiSlice {
				sliceIDMap := findEnvVarPatch("MEGASCALE_SLICE_ID")
				if assert.NotNil(t, sliceIDMap, "MEGASCALE_SLICE_ID missing") {
					assert.Equal(t, fmt.Sprint(tc.expectedReplicaID), sliceIDMap["value"])
				}

				coordMap := findEnvVarPatch("MEGASCALE_COORDINATOR_ADDRESS")
				if assert.NotNil(t, coordMap, "MEGASCALE_COORDINATOR_ADDRESS missing") {
					assert.Equal(t, fmt.Sprintf("%s-0-0.%s-%s", "test-group", "test-cluster", utils.HeadlessServiceSuffix), coordMap["value"])
				}

				portMap := findEnvVarPatch("MEGASCALE_PORT")
				if assert.NotNil(t, portMap, "MEGASCALE_PORT missing") {
					assert.Equal(t, "8081", portMap["value"])
				}
			}
		})
	}
}

func Test_GenerateHeadlessServiceName(t *testing.T) {
	tests := map[string]struct {
		testRayClusterName  string
		expectedServiceName string
	}{
		"RayCluster name + -{HEADLESS_SERVICE_SUFFIX} is less than 50 chars, no truncation": {
			testRayClusterName:  "test-raycluster", // 15 chars
			expectedServiceName: utils.CheckName(fmt.Sprintf("%s-%s", "test-raycluster", utils.HeadlessServiceSuffix)),
		},
		"RayCluster name + -{HEADLESS_SERVICE_SUFFIX} is more than 50 chars, name is truncated": {
			testRayClusterName:  "extremely-really-really-long-test-raycluster-name", // 49 chars
			expectedServiceName: utils.CheckName(fmt.Sprintf("%s-%s", "extremely-really-really-long-test-raycluster-name", utils.HeadlessServiceSuffix)),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			serviceName := generateHeadlessServiceName(tc.testRayClusterName)
			assert.Equal(t, tc.expectedServiceName, serviceName)
		})
	}
}

// getCertFromServer is helper function to fetch the certificate currently being served.
func getCertFromServer(t *testing.T, addr string) *x509.Certificate {
	t.Helper()

	// Create a TLS config for self-signed certificate.
	conf := &tls.Config{InsecureSkipVerify: true}

	conn, err := tls.Dial("tcp", addr, conf)
	if err != nil {
		t.Fatalf("Failed to dial TLS server at %s: %v", addr, err)
	}
	defer conn.Close()

	// Get the certificate chain.
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		t.Fatal("Server did not present any certificates.")
	}
	return certs[0]
}

// generateTestCertKeyPair is a helper function to create a new self-signed cert/key pair for testing.
func generateTestCertKeyPair(t *testing.T, orgName, certPath, keyPath string) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("Failed to generate private key: %v", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("Failed to generate serial number: %v", err)
	}

	// Define the Certificate for test.
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject:      pkix.Name{Organization: []string{orgName}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(5 * time.Minute),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("Failed to create certificate: %v", err)
	}

	// Write certificate file.
	certOut, _ := os.Create(certPath)
	defer certOut.Close()
	pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	// Write key file.
	keyOut, _ := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	defer keyOut.Close()
	privBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes})
}

// TestWebhookCertReloadsOnChange verifies that the server correctly reloads TLS certificate.
func TestWebhookCertReloadsOnChange(t *testing.T) {
	tmpDir := t.TempDir()

	// Set webhook flags.
	CertFile = filepath.Join(tmpDir, "tls.crt")
	KeyFile = filepath.Join(tmpDir, "tls.key")
	BindAddr = "127.0.0.1:45443" // arbitrary unique port

	// Generate the initial certificate and key.
	generateTestCertKeyPair(t, "Test-Cert", CertFile, KeyFile)

	// Start the webhook server.
	go func() {
		err := startServer(NewTPUWebhookServer(nil))
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Logf("Webhook server failed unexpectedly: %v", err)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	// Verify initial certificate.
	initialCert := getCertFromServer(t, BindAddr)
	assert.Equal(t, "Test-Cert", initialCert.Subject.Organization[0], "Unexpected initial certificate")
	t.Logf("Successfully connected and verified initial certificate.")

	// Update certificate file.
	t.Log("Generating and writing reloaded certificate...")
	generateTestCertKeyPair(t, "Reloaded-Cert", CertFile, KeyFile)
	time.Sleep(500 * time.Millisecond) // Give cert-watcher time to reload.

	// Check for the new certificate.
	reloadedCert := getCertFromServer(t, BindAddr)
	assert.Equal(t, "Reloaded-Cert", reloadedCert.Subject.Organization[0], "Expected reloaded cert with new org")

	// Final check to ensure the new cert is different from the old one.
	assert.NotEqual(t, initialCert.SerialNumber, reloadedCert.SerialNumber, "Certificate was not reloaded; serial number is unchanged.")
	t.Logf("Server successfully reloaded the certificate with cert-watcher.")
}
