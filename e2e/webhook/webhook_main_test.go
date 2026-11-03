//go:build e2e || dynamic_slicing

package webhook

import (
	"flag"
	"log"
	"os"
	"testing"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func TestMain(m *testing.M) {
	flag.Parse()
	if ns := os.Getenv("TEST_NAMESPACE"); ns != "" {
		testNamespace = ns
	}

	var err error
	restConfig, err = config.GetConfig()
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
