package webhook

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

var kubeconfig *string

func init() {
	if home := os.Getenv("HOME"); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
}

func TestMain(m *testing.M) {
	flag.Parse()
	os.Exit(m.Run())
}

func TestWebhookMutation_V6eSingleHost(t *testing.T) {
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// Load manifest
	manifestPath := filepath.Join("..", "manifests", "v6e", "v6e-8-single-host.yaml")
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("Error opening manifest file: %v", err)
	}
	defer manifestFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(manifestFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding manifest: %v", err)
	}

	// TODO: Use dynamic client to create cluster instead of assuming it exists.

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	fmt.Printf("Looking for pods with selector: %s\n", labelSelector)

	// Wait for pods
	var pods *corev1.PodList
	for i := 0; i < 5; i++ {
		pods, err = clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			t.Fatalf("Error listing pods: %v", err)
		}
		if len(pods.Items) > 0 {
			break
		}
		fmt.Println("Waiting for pods to be created...")
		time.Sleep(5 * time.Second)
	}

	if len(pods.Items) == 0 {
		t.Skip("No pods found for cluster, skipping verification. Ensure a cluster is running and manifests are applied.")
	}

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
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// Load manifest
	manifestPath := filepath.Join("..", "manifests", "v6e", "v6e-16-multi-host.yaml")
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("Error opening manifest file: %v", err)
	}
	defer manifestFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(manifestFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding manifest: %v", err)
	}

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	fmt.Printf("Looking for pods with selector: %s\n", labelSelector)

	// Wait for pods
	var pods *corev1.PodList
	for i := 0; i < 5; i++ {
		pods, err = clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			t.Fatalf("Error listing pods: %v", err)
		}
		if len(pods.Items) > 0 {
			break
		}
		fmt.Println("Waiting for pods to be created...")
		time.Sleep(5 * time.Second)
	}

	if len(pods.Items) == 0 {
		t.Skip("No pods found for cluster, skipping verification. Ensure a cluster is running and manifests are applied.")
	}

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
			workerId := getEnvVarValue(envVars, "TPU_WORKER_ID")
			tpuName := getEnvVarValue(envVars, "TPU_NAME")

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
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// Load manifest
	manifestPath := filepath.Join("..", "manifests", "v6e", "v6e-16-multi-slice.yaml")
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		t.Fatalf("Error opening manifest file: %v", err)
	}
	defer manifestFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(manifestFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding manifest: %v", err)
	}

	labelSelector := fmt.Sprintf("ray.io/cluster=%s", rayCluster.Name)
	fmt.Printf("Looking for pods with selector: %s\n", labelSelector)

	// Wait for pods
	var pods *corev1.PodList
	for i := 0; i < 5; i++ {
		pods, err = clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			t.Fatalf("Error listing pods: %v", err)
		}
		if len(pods.Items) > 0 {
			break
		}
		fmt.Println("Waiting for pods to be created...")
		time.Sleep(5 * time.Second)
	}

	if len(pods.Items) == 0 {
		t.Skip("No pods found for cluster, skipping verification. Ensure a cluster is running and manifests are applied.")
	}

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

			sliceId := getEnvVarValue(envVars, "MEGASCALE_SLICE_ID")
			sliceIds[sliceId]++

			coordAddr := getEnvVarValue(envVars, "MEGASCALE_COORDINATOR_ADDRESS")
			coordinatorAddresses[coordAddr] = true

			port := getEnvVarValue(envVars, "MEGASCALE_PORT")
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

func TestWebhookMutation_V6ePodChurn(t *testing.T) {
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// We target the existing multi-host cluster
	clusterName := "tpu-v6e-multi-host"
	labelSelector := fmt.Sprintf("ray.io/cluster=%s", clusterName)

	// 1. Get initial pods and track their names
	initialPods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil || len(initialPods.Items) == 0 {
		t.Skip("Skipping pod churn test: No multi-host pods found in default namespace.")
	}

	initialPodNames := make(map[string]bool)
	for _, p := range initialPods.Items {
		initialPodNames[p.Name] = true
	}

	// 2. Find a worker pod and record its TPU_WORKER_ID and name
	var targetPod *corev1.Pod
	var targetWorkerID string
	for _, pod := range initialPods.Items {
		if pod.Labels["ray.io/node-type"] == "worker" {
			targetPod = &pod
			for _, envVar := range pod.Spec.Containers[0].Env {
				if envVar.Name == "TPU_WORKER_ID" {
					targetWorkerID = envVar.Value
					break
				}
			}
			break
		}
	}

	if targetPod == nil || targetWorkerID == "" {
		t.Fatalf("Could not find any active TPU worker pod with a valid TPU_WORKER_ID")
	}

	t.Logf("Targeting worker pod %s (TPU_WORKER_ID=%s) for deletion", targetPod.Name, targetWorkerID)

	// 3. Delete the worker pod
	err = clientset.CoreV1().Pods("default").Delete(context.TODO(), targetPod.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Failed to delete target pod: %v", err)
	}
	t.Log("Target pod deleted. Waiting for KubeRay operator to re-create it...")

	// 4. Poll and wait for the brand-new worker pod (different name than all initial pods) to be created and mutated
	var newPod *corev1.Pod
	for i := 0; i < 15; i++ {
		time.Sleep(5 * time.Second)
		currentPods, err := clientset.CoreV1().Pods("default").List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
		if err != nil {
			t.Fatalf("Failed to list pods during churn polling: %v", err)
		}

		for _, p := range currentPods.Items {
			// Newly created pod should have a name that wasn't in the initial set of pod names
			if p.Labels["ray.io/node-type"] == "worker" && !initialPodNames[p.Name] {
				// Check if this pod has env TPU_WORKER_ID set
				for _, envVar := range p.Spec.Containers[0].Env {
					if envVar.Name == "TPU_WORKER_ID" {
						newPod = &p
						break
					}
				}
			}
		}
		if newPod != nil {
			break
		}
		t.Log("Waiting for re-created pod to be generated and mutated...")
	}

	if newPod == nil {
		t.Fatalf("Timed out waiting for the re-created worker pod to be generated")
	}

	// 5. Assert that the new pod got the SAME TPU_WORKER_ID to fill the gap!
	var assignedWorkerID string
	for _, envVar := range newPod.Spec.Containers[0].Env {
		if envVar.Name == "TPU_WORKER_ID" {
			assignedWorkerID = envVar.Value
			break
		}
	}

	t.Logf("Re-created pod name: %s, Assigned TPU_WORKER_ID: %s", newPod.Name, assignedWorkerID)
	assert.Equal(t, targetWorkerID, assignedWorkerID, "Re-created pod must be assigned the exact same TPU_WORKER_ID to preserve 0 to N-1 sequences during pod churn")
}

func hasEnvVar(envVars []corev1.EnvVar, name string) bool {
	for _, env := range envVars {
		if env.Name == name {
			return true
		}
	}
	return false
}

func getEnvVarValue(envVars []corev1.EnvVar, name string) string {
	for _, env := range envVars {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}
