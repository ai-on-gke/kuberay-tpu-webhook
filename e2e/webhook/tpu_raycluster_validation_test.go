package webhook

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func TestRayClusterValidation_InvalidTopology(t *testing.T) {
	// Setup kube client
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		t.Skipf("Skipping test as kubeconfig is not available: %v", err)
	}

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("Error creating dynamic client: %v", err)
	}

	// Load invalid manifest
	manifestPath := filepath.Join("..", "manifests", "v6e", "v6e-invalid-topology.yaml")
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

	// Convert to Unstructured object for dynamic client creation
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&rayCluster)
	if err != nil {
		t.Fatalf("Error converting RayCluster to unstructured: %v", err)
	}

	gvr := schema.GroupVersionResource{
		Group:    "ray.io",
		Version:  "v1",
		Resource: "rayclusters",
	}

	t.Logf("Attempting to create invalid RayCluster: %s (expecting rejection)", rayCluster.Name)

	_, err = dynamicClient.Resource(gvr).Namespace("default").Create(
		context.TODO(),
		&unstructured.Unstructured{Object: unstructuredObj},
		metav1.CreateOptions{},
	)

	// Assert that creation was rejected by the validating webhook
	assert.Error(t, err, "Expected cluster creation to fail due to topology verification")

	statusErr, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expected StatusError from API server, got: %T (error: %v)", err, err)
	}

	assert.Contains(
		t,
		statusErr.Error(),
		"Number of workers in worker group not equal to specified topology",
		"Validation rejected, but with unexpected error message",
	)

	t.Log("Webhook validation successfully rejected the mismatched RayCluster configuration.")
}
