//go:build e2e

package webhook

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

var rayJobGVR = schema.GroupVersionResource{
	Group:    "ray.io",
	Version:  "v1",
	Resource: "rayjobs",
}

func waitForRayJobSuccess(t *testing.T, jobName string, timeout time.Duration) {
	t.Helper()
	t.Logf("Waiting for RayJob %s to reach SUCCEEDED status...", jobName)

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		unstructJob, err := dynamicClient.Resource(rayJobGVR).Namespace(testNamespace).Get(ctx, jobName, v1.GetOptions{})
		if err != nil {
			return false, err
		}

		status, found, err := unstructured.NestedString(unstructJob.Object, "status", "jobStatus")
		if err != nil || !found {
			t.Logf("RayJob %s status not found yet.", jobName)
			return false, nil
		}

		t.Logf("RayJob %s current status: %s", jobName, status)
		if status == "SUCCEEDED" {
			return true, nil
		}
		if status == "FAILED" {
			return false, fmt.Errorf("RayJob %s failed", jobName)
		}
		return false, nil
	})

	if err != nil {
		t.Fatalf("RayJob %s did not succeed within %v: %v", jobName, timeout, err)
	}
}

func TestRayJobIntegration_V6eJaxTrainer(t *testing.T) {
	waitForRayJobSuccess(t, "v6e-jax-train-rayjob", 15*time.Minute)
}

func TestRayJobIntegration_V7xJaxTrainer(t *testing.T) {
	waitForRayJobSuccess(t, "tpu7x-jax-train-rayjob", 15*time.Minute)
}
