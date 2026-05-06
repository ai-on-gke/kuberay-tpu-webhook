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

func TestWebhookMutation_V6e(t *testing.T) {
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// Load fixture
	fixturePath := filepath.Join("..", "fixtures", "v6e", "v6e-8-single-host.yaml")
	fixtureFile, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("Error opening fixture file: %v", err)
	}
	defer fixtureFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(fixtureFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding fixture: %v", err)
	}

	// TODO: Use dynamic client to create cluster instead of assuming it exists.

	labelSelector := fmt.Sprintf("ray.io/cluster-name=%s", rayCluster.Name)
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
		t.Skip("No pods found for cluster, skipping verification. Ensure a cluster is running and fixtures are applied.")
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

func TestWebhookMutation_V6e_MultiHost(t *testing.T) {
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating clientset: %v", err)
	}

	// Load fixture
	fixturePath := filepath.Join("..", "fixtures", "v6e", "v6e-16-multi-host.yaml")
	fixtureFile, err := os.Open(fixturePath)
	if err != nil {
		t.Fatalf("Error opening fixture file: %v", err)
	}
	defer fixtureFile.Close()

	var rayCluster rayv1.RayCluster
	decoder := yaml.NewYAMLOrJSONDecoder(fixtureFile, 1024)
	if err := decoder.Decode(&rayCluster); err != nil {
		t.Fatalf("Error decoding fixture: %v", err)
	}

	labelSelector := fmt.Sprintf("ray.io/cluster-name=%s", rayCluster.Name)
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
		t.Skip("No pods found for cluster, skipping verification. Ensure a cluster is running and fixtures are applied.")
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
