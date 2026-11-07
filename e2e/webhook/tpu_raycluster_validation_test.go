//go:build e2e

package webhook

import (
	"testing"
)

func TestRayClusterValidation_InvalidTopology(t *testing.T) {
	baseCluster := loadManifest(t, "../manifests/invalid/invalid-topology.yaml")

	t.Log("Running validating webhook case: Strict Topology Mismatch Rejection")
	// Base invalid-topology manifest has replica workerGroup with numOfHosts: 2
	// but requests 2x4 topology (expected 1 host)
	assertRayClusterRejected(t, baseCluster, errTopologyMismatch)
}

func TestRayClusterValidation_MissingTopologyKey(t *testing.T) {
	cluster := loadManifest(t, "../manifests/invalid/invalid-topology.yaml")
	cluster.Name = "tpu-v6e-missing-topology"

	t.Log("Running validating webhook case: Missing cloud.google.com/gke-tpu-topology Selector Rejection")
	// Remove topology nodeSelector key entirely
	delete(cluster.Spec.WorkerGroupSpecs[0].Template.Spec.NodeSelector, "cloud.google.com/gke-tpu-topology")

	// Since missing node selectors trigger an internal parsing error in checkWorkersMatchTopology,
	// the webhook controller terminates with an admission crash.
	// We assert that the API server returns a clean calling webhook crash payload.
	assertRayClusterRejected(t, cluster, "Failed to validate RayCluster")
}

func TestRayClusterValidation_NumOfHostsZero(t *testing.T) {
	cluster := loadManifest(t, "../manifests/invalid/invalid-num-of-hosts-zero.yaml")
	t.Log("Running validating webhook case: NumOfHosts=0 Rejection")
	assertRayClusterRejected(t, cluster, errTopologyMismatch)
}

func TestRayClusterValidation_NumOfHostsOmitted(t *testing.T) {
	cluster := loadManifest(t, "../manifests/invalid/invalid-num-of-hosts-omitted.yaml")
	t.Log("Running validating webhook case: NumOfHosts omitted Rejection")
	assertRayClusterRejected(t, cluster, errTopologyMismatch)
}

func TestRayClusterValidation_DynamicSliceDefaultScheduler_RejectsTopologyMismatch(t *testing.T) {
	cluster := loadManifest(t, "../manifests/invalid/invalid-topology.yaml")
	cluster.Name = "tpu-ds-slice-invalid-topology"
	wg := &cluster.Spec.WorkerGroupSpecs[0]
	delete(wg.Template.Spec.NodeSelector, "cloud.google.com/gke-tpu-topology")
	wg.Template.Spec.NodeSelector["cloud.google.com/gke-tpu-slice"] = "slice-0"
	if wg.Template.Annotations == nil {
		wg.Template.Annotations = make(map[string]string)
	}
	wg.Template.Annotations["cloud.google.com/gke-tpu-slice-topology"] = "2x4"
	// 2x4 with 8 TPUs/host expects 1 host, so NumOfHosts=2 must still be rejected.
	wg.NumOfHosts = 2

	t.Log("Running validating webhook case: Dynamic Slicing gke-tpu-slice NumOfHosts mismatch rejection")
	assertRayClusterRejected(t, cluster, errTopologyMismatch)
}
