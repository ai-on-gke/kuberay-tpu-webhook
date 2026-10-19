//go:build e2e

package webhook

import (
	"testing"
	"time"
)

func TestRayJobIntegration_V6eJaxTrainer(t *testing.T) {
	waitForRayJobSuccess(t, "v6e-jax-train-rayjob", 15*time.Minute)
}

func TestRayJobIntegration_V7xJaxTrainer(t *testing.T) {
	waitForRayJobSuccess(t, "tpu7x-jax-train-rayjob", 15*time.Minute)
}
