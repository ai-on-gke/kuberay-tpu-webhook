package webhook

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var (
	kubeconfig    *string
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
	initErr       error
)

func init() {
	if home := os.Getenv("HOME"); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err == nil {
		clientset, err = kubernetes.NewForConfig(config)
		if err == nil {
			dynamicClient, err = dynamic.NewForConfig(config)
		}
	}
	initErr = err
	os.Exit(m.Run())
}

func TestWebhookMutation_V6eSingleHost(t *testing.T) {
	if initErr != nil {
		t.Skipf("Skipping test as cluster clients could not be initialized: %v", initErr)
	}

	// Load manifest
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-8-single-host.yaml")

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	t.Logf("Looking for pods with selector: %s", labelSelector)

	// Wait for pods
	pods := waitForPods(t, labelSelector)

	// Verify mutations
	for _, pod := range pods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			// Verify env vars
			envVars := pod.Spec.Containers[0].Env
			assert.True(t, hasEnvVar(envVars, "TPU_WORKER_ID"), "Missing TPU_WORKER_ID")
			assert.True(t, hasEnvVar(envVars, "TPU_NAME"), "Missing TPU_NAME")
			assert.True(t, hasEnvVar(envVars, "TPU_DEVICE_PLUGIN_HOST_IP"), "Missing TPU_DEVICE_PLUGIN_HOST_IP")
			assert.True(t, hasEnvVar(envVars, "TPU_DEVICE_PLUGIN_ADDR"), "Missing TPU_DEVICE_PLUGIN_ADDR")
			break
		}
	}
}

func TestWebhookMutation_V6eMultiHost(t *testing.T) {
	if initErr != nil {
		t.Skipf("Skipping test as cluster clients could not be initialized: %v", initErr)
	}

	// Load manifest
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-16-multi-host.yaml")

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	t.Logf("Looking for pods with selector: %s", labelSelector)

	// Wait for pods
	pods := waitForPods(t, labelSelector)

	// Collect values
	workerIds := make(map[string]bool)
	tpuNames := make(map[string]bool)
	replicaIndices := make(map[string]bool)

	numWorkerPods := 0
	for _, pod := range pods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			// Extract values
			workerId := envVarValue(envVars, "TPU_WORKER_ID")
			tpuName := envVarValue(envVars, "TPU_NAME")

			assert.NotEmpty(t, workerId, "TPU_WORKER_ID is empty")
			assert.NotEmpty(t, tpuName, "TPU_NAME is empty")

			workerIds[workerId] = true
			tpuNames[tpuName] = true

			// Check labels
			replicaIndex := pod.Labels["replicaIndex"]
			assert.NotEmpty(t, replicaIndex, "replicaIndex label is missing")
			replicaIndices[replicaIndex] = true

			// Verify subdomain and hostname
			assert.NotEmpty(t, pod.Spec.Subdomain, "Subdomain not set")
			assert.NotEmpty(t, pod.Spec.Hostname, "Hostname not set")

			// Verify Pod Affinity
			assert.NotNil(t, pod.Spec.Affinity, "Affinity not set")
			assert.NotNil(t, pod.Spec.Affinity.PodAntiAffinity, "PodAntiAffinity not set")
		}
	}

	// Assertions
	assert.Equal(t, 4, numWorkerPods, "Expected 4 worker pods for v6e multi-host fixture")
	assert.Equal(t, 4, len(workerIds), "TPU_WORKER_ID values are not unique")

	for i := 0; i < 4; i++ {
		assert.True(t, workerIds[fmt.Sprint(i)], "Missing TPU_WORKER_ID %d", i)
	}

	assert.Equal(t, 1, len(tpuNames), "All pods in the same slice should share the same TPU_NAME")
	assert.Equal(t, 1, len(replicaIndices), "All pods in the same slice should share the same replicaIndex")
}

func TestWebhookMutation_V6eMultiSlice(t *testing.T) {
	if initErr != nil {
		t.Skipf("Skipping test as cluster clients could not be initialized: %v", initErr)
	}

	// Load manifest
	rayCluster := loadManifest(t, "../manifests/v6e/v6e-16-multi-slice.yaml")

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	t.Logf("Looking for pods with selector: %s", labelSelector)

	// Wait for pods
	pods := waitForPods(t, labelSelector)

	// Collect multi-slice validation values
	numWorkerPods := 0
	sliceIds := make(map[string]int)
	coordinatorAddresses := make(map[string]bool)
	megascalePorts := make(map[string]bool)

	for _, pod := range pods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			numWorkerPods++
			envVars := pod.Spec.Containers[0].Env

			assert.True(t, hasEnvVar(envVars, "MEGASCALE_SLICE_ID"), "Missing MEGASCALE_SLICE_ID")
			assert.True(t, hasEnvVar(envVars, "MEGASCALE_COORDINATOR_ADDRESS"), "Missing MEGASCALE_COORDINATOR_ADDRESS")
			assert.True(t, hasEnvVar(envVars, "MEGASCALE_PORT"), "Missing MEGASCALE_PORT")

			sliceId := envVarValue(envVars, "MEGASCALE_SLICE_ID")
			sliceIds[sliceId]++

			coordAddr := envVarValue(envVars, "MEGASCALE_COORDINATOR_ADDRESS")
			coordinatorAddresses[coordAddr] = true

			port := envVarValue(envVars, "MEGASCALE_PORT")
			megascalePorts[port] = true
		}
	}

	// Assertions
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
	if initErr != nil {
		t.Skipf("Skipping test as cluster clients could not be initialized: %v", initErr)
	}

	clusterName := "tpu-v6e-multi-host"
	labelSelector := fmt.Sprintf("ray.io/cluster=%s", clusterName)

	// 1. Get initial pods and track their names
	initialPods, err := clientset.CoreV1().Pods("default").List(t.Context(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(initialPods.Items) == 0 {
		t.Skip("Skipping pod churn test: No multi-host pods found in default namespace.")
	}

	initialPodNames := make(map[string]bool)
	for _, p := range initialPods.Items {
		initialPodNames[p.Name] = true
	}

	// 2. Select two worker pods (half the slice) to delete concurrently
	var targetPods []corev1.Pod
	for _, pod := range initialPods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
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
		wID := envVarValue(pod.Spec.Containers[0].Env, "TPU_WORKER_ID")
		expectedWorkerIDs[wID] = true
		t.Logf("Targeting worker pod %s (TPU_WORKER_ID=%s) for deletion", pod.Name, wID)
	}

	// 3. Delete both worker pods concurrently
	var wg sync.WaitGroup
	for _, pod := range targetPods {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			err := clientset.CoreV1().Pods("default").Delete(t.Context(), podName, metav1.DeleteOptions{})
			if err != nil {
				t.Errorf("Failed to delete pod %s: %v", podName, err)
			}
		}(pod.Name)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	t.Log("Target pods deleted concurrently. Waiting for KubeRay operator to re-create both...")

	// 4. Poll and wait for the two brand-new worker pods to be created and mutated
	var recreatedPods []corev1.Pod
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	err = wait.PollUntilContextTimeout(ctx, 3*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		currentPods, err := clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}

		recreatedPods = nil
		for _, p := range currentPods.Items {
			if p.Labels["ray.io/node-type"] == "worker" && !initialPodNames[p.Name] {
				if hasEnvVar(p.Spec.Containers[0].Env, "TPU_WORKER_ID") {
					recreatedPods = append(recreatedPods, p)
				}
			}
		}
		if len(recreatedPods) == 2 {
			return true, nil
		}
		t.Logf("Waiting for recreated pods to be mutated... (found %d/2)", len(recreatedPods))
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for the recreated worker pods: %v (found %d/2)", err, len(recreatedPods))
	}

	// 5. Assert that the new pods got the same TPU_WORKER_IDs as before the churn
	for _, pod := range recreatedPods {
		assignedID := envVarValue(pod.Spec.Containers[0].Env, "TPU_WORKER_ID")
		t.Logf("Re-created pod name: %s, Assigned TPU_WORKER_ID: %s", pod.Name, assignedID)
		assert.True(t, expectedWorkerIDs[assignedID], "Re-created pod TPU_WORKER_ID %s was not in the original expected IDs", assignedID)
	}
}

func TestWebhookMutation_V6ePodChurnMultiSlice(t *testing.T) {
	if initErr != nil {
		t.Skipf("Skipping test as cluster clients could not be initialized: %v", initErr)
	}

	clusterName := "tpu-v6e-multi-slice"
	labelSelector := fmt.Sprintf("ray.io/cluster=%s", clusterName)

	// 1. Get initial pods and track their names
	initialPods, err := clientset.CoreV1().Pods("default").List(t.Context(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(initialPods.Items) == 0 {
		t.Skip("Skipping pod churn test: No multi-slice pods found in default namespace.")
	}

	initialPodNames := make(map[string]bool)
	for _, p := range initialPods.Items {
		initialPodNames[p.Name] = true
	}

	// 2. Target four worker pods: two in slice 0, and two in slice 1
	var targetPods []corev1.Pod
	var slice0Targets, slice1Targets []corev1.Pod

	for _, pod := range initialPods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			sliceID := envVarValue(pod.Spec.Containers[0].Env, "MEGASCALE_SLICE_ID")
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
		sliceID := envVarValue(pod.Spec.Containers[0].Env, "MEGASCALE_SLICE_ID")
		wID := envVarValue(pod.Spec.Containers[0].Env, "TPU_WORKER_ID")
		expectedPodMappings[sliceID][wID] = true
		t.Logf("Targeting multi-slice worker pod %s (Slice=%s, TPU_WORKER_ID=%s) for deletion", pod.Name, sliceID, wID)
	}

	// 3. Delete all four worker pods concurrently
	var wg sync.WaitGroup
	for _, pod := range targetPods {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			err := clientset.CoreV1().Pods("default").Delete(t.Context(), podName, metav1.DeleteOptions{})
			if err != nil {
				t.Errorf("Failed to delete pod %s: %v", podName, err)
			}
		}(pod.Name)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	t.Log("Target pods deleted concurrently. Waiting for KubeRay operator to re-create all four...")

	// 4. Poll and wait for all four new worker pods to be created and mutated
	var recreatedPods []corev1.Pod
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	err = wait.PollUntilContextTimeout(ctx, 3*time.Second, 90*time.Second, true, func(ctx context.Context) (bool, error) {
		currentPods, err := clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}

		recreatedPods = nil
		for _, p := range currentPods.Items {
			if p.Labels["ray.io/node-type"] == "worker" && !initialPodNames[p.Name] {
				if hasEnvVar(p.Spec.Containers[0].Env, "TPU_WORKER_ID") && hasEnvVar(p.Spec.Containers[0].Env, "MEGASCALE_SLICE_ID") {
					recreatedPods = append(recreatedPods, p)
				}
			}
		}
		if len(recreatedPods) == 4 {
			return true, nil
		}
		t.Logf("Waiting for recreated multi-slice pods... (found %d/4)", len(recreatedPods))
		return false, nil
	})
	if err != nil {
		t.Fatalf("Timed out waiting for the recreated worker pods in multi-slice cluster: %v (found %d/4)", err, len(recreatedPods))
	}

	// 5. Assert that each recreated pod got its original TPU_WORKER_ID under the correct MEGASCALE_SLICE_ID
	for _, pod := range recreatedPods {
		sliceID := envVarValue(pod.Spec.Containers[0].Env, "MEGASCALE_SLICE_ID")
		assignedID := envVarValue(pod.Spec.Containers[0].Env, "TPU_WORKER_ID")
		t.Logf("Re-created multi-slice pod name: %s, Assigned Slice: %s, TPU_WORKER_ID: %s", pod.Name, sliceID, assignedID)

		originalExpectedIDs, exists := expectedPodMappings[sliceID]
		assert.True(t, exists, "Recreated pod assigned to unexpected sliceID: %s", sliceID)
		assert.True(t, originalExpectedIDs[assignedID], "Recreated pod in slice %s with TPU_WORKER_ID %s was not in the original expected set", sliceID, assignedID)
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

func waitForPods(t *testing.T, labelSelector string) *corev1.PodList {
	t.Helper()
	var pods *corev1.PodList
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		var err error
		pods, err = clientset.CoreV1().Pods("default").List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			return false, err
		}
		if len(pods.Items) > 0 {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("Error waiting for pods with selector %s: %v", labelSelector, err)
	}
	return pods
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
