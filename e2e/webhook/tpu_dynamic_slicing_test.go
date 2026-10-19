//go:build dynamic_slicing

package webhook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"
	"time"

	rayv1 "github.com/ray-project/kuberay/ray-operator/apis/ray/v1"
	"github.com/ray-project/kuberay/ray-operator/controllers/ray/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	// GKE TPU Dynamic Slicing labels and annotations.
	gkeSliceTopologyAnnotation    = "cloud.google.com/gke-tpu-slice-topology"
	gkeTPUAcceleratorLabel        = "cloud.google.com/gke-tpu-accelerator"
	gkeTPUTopologyLabel           = "cloud.google.com/gke-tpu-topology"
	gkeNodePoolLabel              = "cloud.google.com/gke-nodepool"
	gceTopologyBlockLabel         = "cloud.google.com/gce-topology-block"
	gkePartition4x4x4IDLabel      = "cloud.google.com/gke-tpu-partition-4x4x4-id"
	skipTPUWebhookCheckAnnotation = "cloud.google.com/skip-tpu-webhook-check"

	// Kueue labels and TAS annotations.
	kueueQueueLabel                             = "kueue.x-k8s.io/queue-name"
	kueuePodSetRequiredTopology                 = "kueue.x-k8s.io/podset-required-topology"
	kueuePodSetPreferredTopology                = "kueue.x-k8s.io/podset-preferred-topology"
	kueuePodSetSliceRequiredTopology            = "kueue.x-k8s.io/podset-slice-required-topology"
	kueuePodSetSliceRequiredTopologyConstraints = "kueue.x-k8s.io/podset-slice-required-topology-constraints"
	kueuePodSetSliceSize                        = "kueue.x-k8s.io/podset-slice-size"
	kueuePodSetUnconstrainedTopology            = "kueue.x-k8s.io/podset-unconstrained-topology"
	kueueTopologySchedulingGate                 = "kueue.x-k8s.io/topology"

	// GKE Slice CR annotations, labels, and conditions.
	sliceOwnerWorkloadNameAnnotation      = "accelerator.gke.io/owner-workload-name"
	sliceOwnerWorkloadNamespaceAnnotation = "accelerator.gke.io/owner-workload-namespace"
	sliceOwnerPodSetNameAnnotation        = "accelerator.gke.io/owner-podset-name"
	sliceUsedByNamespaceLabel             = "kueue.gke.io/tpu-slice-used-by-namespace"
	sliceUsedByWorkloadLabel              = "kueue.gke.io/tpu-slice-used-by-workload"
	sliceInUseConditionType               = "KueueAcInUse"

	// Default Kueue queue names on dynamic-slicing clusters.
	defaultDSLocalQueueName   = "lq"
	defaultDSClusterQueueName = "cq"
)

var (
	workloadGVR = schema.GroupVersionResource{
		Group:    "kueue.x-k8s.io",
		Version:  "v1beta2",
		Resource: "workloads",
	}
	sliceGVR = schema.GroupVersionResource{
		Group:    "accelerator.gke.io",
		Version:  "v1beta1",
		Resource: "slices",
	}
	localQueueGVR = schema.GroupVersionResource{
		Group:    "kueue.x-k8s.io",
		Version:  "v1beta2",
		Resource: "localqueues",
	}
)

func dsLocalQueueName() string {
	if v := os.Getenv("DYNAMIC_SLICING_LOCAL_QUEUE"); v != "" {
		return v
	}
	return defaultDSLocalQueueName
}

func dsClusterQueueName() string {
	if v := os.Getenv("DYNAMIC_SLICING_CLUSTER_QUEUE"); v != "" {
		return v
	}
	return defaultDSClusterQueueName
}

// ensureLocalQueue ensures that the target LocalQueue exists in testNamespace
// and points to the configured ClusterQueue.
func ensureLocalQueue(t *testing.T) {
	t.Helper()
	lqName := dsLocalQueueName()
	cqName := dsClusterQueueName()

	_, err := dynamicClient.Resource(localQueueGVR).Namespace(testNamespace).
		Get(t.Context(), lqName, metav1.GetOptions{})
	if err == nil {
		return
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Failed checking LocalQueue %s/%s: %v", testNamespace, lqName, err)
	}

	t.Logf("Creating LocalQueue %s/%s pointing to ClusterQueue %s...", testNamespace, lqName, cqName)
	lq := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "kueue.x-k8s.io/v1beta2",
			"kind":       "LocalQueue",
			"metadata": map[string]any{
				"name":      lqName,
				"namespace": testNamespace,
			},
			"spec": map[string]any{
				"clusterQueue": cqName,
			},
		},
	}
	_, err = dynamicClient.Resource(localQueueGVR).Namespace(testNamespace).
		Create(t.Context(), lq, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Failed creating LocalQueue %s/%s: %v", testNamespace, lqName, err)
	}
}

func applyRayImageOverride(cluster *rayv1.RayCluster) {
	img := os.Getenv("RAY_TPU7X_IMAGE")
	if img == "" {
		return
	}
	for i := range cluster.Spec.HeadGroupSpec.Template.Spec.Containers {
		cluster.Spec.HeadGroupSpec.Template.Spec.Containers[i].Image = img
	}
	for i := range cluster.Spec.WorkerGroupSpecs {
		for j := range cluster.Spec.WorkerGroupSpecs[i].Template.Spec.Containers {
			cluster.Spec.WorkerGroupSpecs[i].Template.Spec.Containers[j].Image = img
		}
	}
}

func applyQueueOverride(labels map[string]string) map[string]string {
	if labels == nil {
		labels = make(map[string]string)
	}
	if _, ok := labels[kueueQueueLabel]; ok {
		labels[kueueQueueLabel] = dsLocalQueueName()
	}
	return labels
}

// waitForClusterCleanup waits until the RayCluster/RayJob, its Pods, its Kueue Workload,
// and any active Slice CRs owned by it are completely removed from the cluster.
func waitForClusterCleanup(t *testing.T, ownerKind, ownerName string) {
	t.Helper()
	waitForClusterCleanupWithContext(t.Context(), t, ownerKind, ownerName)
}

func waitForClusterCleanupWithContext(ctx context.Context, t *testing.T, ownerKind, ownerName string) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		ctx, 3*time.Second, 4*time.Minute, true, func(ctx context.Context) (bool, error) {
			// 1. Check owner object is gone.
			gvr := rayClusterGVR
			if ownerKind == "RayJob" {
				gvr = rayJobGVR
			}
			_, err := dynamicClient.Resource(gvr).Namespace(testNamespace).Get(ctx, ownerName, metav1.GetOptions{})
			if err == nil {
				return false, nil
			}
			if !apierrors.IsNotFound(err) {
				return false, err
			}

			// 2. Check pods in testNamespace for this cluster prefix are gone.
			pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			for _, p := range pods.Items {
				clusterLabel := p.Labels[utils.RayClusterLabelKey]
				if clusterLabel == ownerName || strings.HasPrefix(clusterLabel, ownerName+"-") ||
					strings.HasPrefix(p.Name, ownerName+"-") {
					return false, nil
				}
			}

			// 3. Check Kueue Workloads owned by this object are gone.
			wls, err := dynamicClient.Resource(workloadGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return false, err
				}
			} else {
				for _, wl := range wls.Items {
					for _, ref := range wl.GetOwnerReferences() {
						if ref.Kind == ownerKind && ref.Name == ownerName {
							return false, nil
						}
					}
				}
			}

			// 4. Check Slice CRs (accelerator.gke.io/v1beta1) owned by this object in testNamespace are gone.
			slices, err := dynamicClient.Resource(sliceGVR).List(ctx, metav1.ListOptions{})
			if err != nil {
				if !apierrors.IsNotFound(err) {
					return false, err
				}
			} else {
				for _, s := range slices.Items {
					annotations := s.GetAnnotations()
					if annotations != nil && annotations[sliceOwnerWorkloadNamespaceAnnotation] == testNamespace {
						ownerWl := annotations[sliceOwnerWorkloadNameAnnotation]
						if strings.Contains(ownerWl, ownerName) {
							return false, nil
						}
					}
				}
			}

			return true, nil
		})
	if err != nil {
		require.NoError(t, err,
			"cleanup wait for %s %s/%s timed out, leaking cluster resources", ownerKind, testNamespace, ownerName)
	}
}

// createRayClusterFromManifest loads a RayCluster YAML manifest, creates it in testNamespace,
// registers automatic cleanup, and returns the mutated RayCluster stored on the API server.
func createRayClusterFromManifest(t *testing.T, relativePath string) *rayv1.RayCluster {
	t.Helper()
	ensureLocalQueue(t)

	cluster := loadManifest(t, relativePath)
	cluster.Namespace = testNamespace
	cluster.Labels = applyQueueOverride(cluster.Labels)
	applyRayImageOverride(cluster)

	// Clean up any leftover cluster with the same name before creating.
	_ = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
		Delete(t.Context(), cluster.Name, metav1.DeleteOptions{})
	waitForClusterCleanup(t, "RayCluster", cluster.Name)

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	require.NoError(t, err, "Failed converting RayCluster to unstructured")

	t.Logf("Creating RayCluster %s/%s from %s...", testNamespace, cluster.Name, relativePath)
	createdUnstruct, err := dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
		Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{})
	require.NoError(t, err, "Failed creating RayCluster %s", cluster.Name)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		t.Logf("Cleaning up RayCluster %s/%s...", testNamespace, cluster.Name)
		_ = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Delete(cleanupCtx, cluster.Name, metav1.DeleteOptions{})
		waitForClusterCleanupWithContext(cleanupCtx, t, "RayCluster", cluster.Name)
	})

	var createdCluster rayv1.RayCluster
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(createdUnstruct.Object, &createdCluster)
	require.NoError(t, err, "Failed converting created unstructured object back to RayCluster")
	return &createdCluster
}

func loadRayJobManifest(t *testing.T, relativePath string) *rayv1.RayJob {
	t.Helper()
	_, filename, _, ok := stdruntime.Caller(0)
	if !ok {
		t.Fatalf("Failed to get current file path via runtime.Caller")
	}
	manifestPath := filepath.Clean(filepath.Join(filepath.Dir(filename), relativePath))

	var rayJob rayv1.RayJob
	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err, "Failed reading RayJob manifest %s", manifestPath)
	err = yaml.Unmarshal(data, &rayJob)
	require.NoError(t, err, "Failed decoding RayJob manifest %s", manifestPath)
	return &rayJob
}

// waitForWorkloadAdmitted waits for the Kueue Workload owned by (ownerKind, ownerName)
// to be created and reach Admitted=True.
func waitForWorkloadAdmitted(
	t *testing.T, ownerKind, ownerName string, timeout time.Duration,
) *unstructured.Unstructured {
	t.Helper()
	t.Logf("Waiting for Kueue Workload of %s %s/%s to be admitted...", ownerKind, testNamespace, ownerName)

	var admittedWL *unstructured.Unstructured
	err := wait.PollUntilContextTimeout(
		t.Context(), 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			wls, err := dynamicClient.Resource(workloadGVR).Namespace(testNamespace).List(ctx, metav1.ListOptions{})
			if err != nil {
				return false, err
			}
			for i := range wls.Items {
				wl := &wls.Items[i]
				matchedOwner := false
				for _, ref := range wl.GetOwnerReferences() {
					if ref.Kind == ownerKind && ref.Name == ownerName {
						matchedOwner = true
						break
					}
				}
				if !matchedOwner {
					continue
				}

				conditions, found, _ := unstructured.NestedSlice(wl.Object, "status", "conditions")
				if !found {
					t.Logf("Workload %s found, waiting for conditions...", wl.GetName())
					return false, nil
				}
				for _, c := range conditions {
					condMap, ok := c.(map[string]any)
					if !ok {
						continue
					}
					if condMap["type"] == "Admitted" && condMap["status"] == "True" {
						admittedWL = wl.DeepCopy()
						return true, nil
					}
					if condMap["type"] == "Admitted" {
						t.Logf("Workload %s Admitted=%v (reason=%v, message=%v)",
							wl.GetName(), condMap["status"], condMap["reason"], condMap["message"])
					}
				}
			}
			return false, nil
		})
	require.NoError(t, err, "Timed out waiting for Kueue Workload of %s %s to be admitted", ownerKind, ownerName)
	return admittedWL
}

// findSlicesForWorkload lists all Slice CRs currently owned by workloadName in testNamespace.
func findSlicesForWorkload(ctx context.Context, workloadName string) ([]unstructured.Unstructured, error) {
	sliceList, err := dynamicClient.Resource(sliceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var matched []unstructured.Unstructured
	for _, s := range sliceList.Items {
		annotations := s.GetAnnotations()
		if annotations[sliceOwnerWorkloadNamespaceAnnotation] == testNamespace &&
			annotations[sliceOwnerWorkloadNameAnnotation] == workloadName {
			matched = append(matched, s)
		}
	}
	return matched, nil
}

// waitForSlicesActive polls until expectedCount Slice CRs for workloadName exist and have Ready=True.
func waitForSlicesActive(
	t *testing.T, workloadName string, expectedCount int, timeout time.Duration,
) []unstructured.Unstructured {
	t.Helper()
	t.Logf("Waiting for %d Slice CR(s) of Workload %s to become ACTIVE...", expectedCount, workloadName)

	var activeSlices []unstructured.Unstructured
	err := wait.PollUntilContextTimeout(
		t.Context(), 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			matched, err := findSlicesForWorkload(ctx, workloadName)
			if err != nil {
				return false, err
			}
			if len(matched) < expectedCount {
				t.Logf("Found %d/%d Slice CR(s) for Workload %s...", len(matched), expectedCount, workloadName)
				return false, nil
			}

			allReady := true
			for _, s := range matched {
				conds, _, _ := unstructured.NestedSlice(s.Object, "status", "conditions")
				ready := false
				reason := ""
				for _, c := range conds {
					cm, ok := c.(map[string]any)
					if !ok {
						continue
					}
					if cm["type"] == "Ready" {
						reason, _ = cm["reason"].(string)
						if cm["status"] == "True" {
							ready = true
						}
					}
				}
				t.Logf("Slice %s: Ready=%t (reason=%s)", s.GetName(), ready, reason)
				if !ready {
					allReady = false
				}
			}
			if allReady && len(matched) == expectedCount {
				activeSlices = matched
				return true, nil
			}
			return false, nil
		})
	require.NoError(t, err,
		"Timed out waiting for %d Slice CR(s) of Workload %s to become ACTIVE", expectedCount, workloadName)
	return activeSlices
}

func assertNoSyntheticPodAffinity(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.Affinity == nil {
		return
	}
	assert.Nil(t, pod.Spec.Affinity.PodAffinity,
		"Pod %s should not have synthetic PodAffinity when managed by Dynamic Slicing / Kueue TAS", pod.Name)
	assert.Nil(t, pod.Spec.Affinity.PodAntiAffinity,
		"Pod %s should not have synthetic PodAntiAffinity when managed by Dynamic Slicing / Kueue TAS", pod.Name)
}

func assertSliceHealthNodeAffinity(t *testing.T, affinity *corev1.Affinity, expectedHealthKey string) {
	t.Helper()
	require.NotNil(t, affinity, "Expected Affinity to be set by kueue-slice-controller")
	require.NotNil(t, affinity.NodeAffinity, "Expected NodeAffinity to be set by kueue-slice-controller")
	require.NotNil(t, affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		"Expected RequiredDuringSchedulingIgnoredDuringExecution NodeAffinity")

	found := false
	for _, term := range affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == expectedHealthKey {
				found = true
				assert.Equal(t, corev1.NodeSelectorOpIn, expr.Operator)
				assert.ElementsMatch(t, []string{"HEALTHY", "DEGRADED"}, expr.Values)
			}
		}
	}
	assert.True(t, found, "Expected NodeAffinity requirement for %s In [HEALTHY, DEGRADED]", expectedHealthKey)
}

func assertTorchTpuEnvsForV7x(
	t *testing.T, envVars []corev1.EnvVar, expectedTorchTopology string, numHosts, chipsPerHost int,
) {
	t.Helper()
	assert.Equal(t, expectedTorchTopology, envVarValue(envVars, "TORCH_TPU_TOPOLOGY"),
		"Unexpected TORCH_TPU_TOPOLOGY (should be derived from gke-tpu-slice-topology)")

	sliceBuilderAddrs := envVarValue(envVars, "TORCH_TPU_SLICEBUILDER_ADDRESSES")
	assert.NotEmpty(t, sliceBuilderAddrs, "TORCH_TPU_SLICEBUILDER_ADDRESSES should be populated")
	parts := strings.Split(sliceBuilderAddrs, ",")
	// On tpu7x (TensorNode dual-chiplet), ports per host = chipsPerHost * 2.
	expectedAddrCount := numHosts * chipsPerHost * 2
	assert.Len(t, parts, expectedAddrCount,
		"Unexpected number of endpoints in TORCH_TPU_SLICEBUILDER_ADDRESSES")
}

// runDistributedJAXCheck uploads and runs a Python script on the Ray head pod that
// schedules a Ray task on every TPU worker host in the slice and runs jax.distributed.initialize() + matmul.
// On TPU7x (dual-chiplet TensorNode), each TPU chip exposes 2 JAX devices, so expectedLocalDevices = chipsPerHost * 2
// and expectedGlobalDevices = numHosts * chipsPerHost * 2.
func runDistributedJAXCheck(
	t *testing.T, headPod *corev1.Pod, numHosts, chipsPerHost, expectedLocalDevices, expectedGlobalDevices int,
) {
	t.Helper()
	script := fmt.Sprintf(`import os
import ray
from ray.util.placement_group import placement_group
from ray.util.scheduling_strategies import PlacementGroupSchedulingStrategy

ray.init(address="auto")

num_hosts = %d
chips_per_host = %d
expected_local_devices = %d
expected_global_devices = %d

tpu_nodes = [n for n in ray.nodes() if n.get("Alive") and n.get("Resources", {}).get("TPU", 0) >= chips_per_host]
print(f"Discovered {len(tpu_nodes)} alive TPU nodes in Ray GCS: {[n.get('NodeManagerAddress') for n in tpu_nodes]}")
assert len(tpu_nodes) == num_hosts, f"Expected {num_hosts} TPU nodes in Ray, got {len(tpu_nodes)}"

pg = placement_group([{"TPU": chips_per_host} for _ in range(num_hosts)], strategy="STRICT_SPREAD")
ray.get(pg.ready(), timeout=60)

@ray.remote(num_cpus=0, resources={"TPU": chips_per_host})
def check_jax_worker():
    import jax
    import jax.numpy as jnp
    worker_id = os.environ.get("TPU_WORKER_ID")
    tpu_name = os.environ.get("TPU_NAME")
    assert worker_id is not None, "TPU_WORKER_ID missing"
    assert tpu_name is not None, "TPU_NAME missing"

    jax.distributed.initialize()
    global_devs = jax.device_count()
    local_devs = jax.local_device_count()
    assert global_devs == expected_global_devices, (
        f"Expected {expected_global_devices} global devices, got {global_devs}"
    )
    assert local_devs == expected_local_devices, f"Expected {expected_local_devices} local devices, got {local_devs}"

    x = jax.random.normal(jax.random.PRNGKey(0), (1024, 1024), dtype=jnp.bfloat16)
    y = jax.random.normal(jax.random.PRNGKey(1), (1024, 1024), dtype=jnp.bfloat16)
    z = jnp.matmul(x, y)
    z.block_until_ready()
    return {
        "hostname": os.uname().nodename,
        "worker_id": worker_id,
        "tpu_name": tpu_name,
        "global_devices": global_devs,
        "local_devices": local_devs,
    }

tasks = [
    check_jax_worker.options(
        scheduling_strategy=PlacementGroupSchedulingStrategy(placement_group=pg, placement_group_bundle_index=i)
    ).remote()
    for i in range(num_hosts)
]
results = ray.get(tasks, timeout=180)
print("JAX worker results:", results)
worker_ids = sorted(int(r["worker_id"]) for r in results)
assert worker_ids == list(range(num_hosts)), f"Unexpected worker IDs: {worker_ids}"
print("DYNAMIC_SLICING_JAX_CHECK_PASSED")
`, numHosts, chipsPerHost, expectedLocalDevices, expectedGlobalDevices)

	remotePath := "/tmp/verify_ds_jax.py"
	writeCmd := []string{"bash", "-c", "cat << 'EOF' > " + remotePath + "\n" + script + "\nEOF\n"}
	stdout, stderr, err := execCommandInPod(t, headPod.Name, headPod.Spec.Containers[0].Name, writeCmd)
	require.NoError(t, err, "Failed writing JAX check script to head pod (stdout=%q, stderr=%q)", stdout, stderr)

	t.Logf("Executing distributed JAX check across %d hosts (%d global devices)...", numHosts, expectedGlobalDevices)
	stdout, stderr, err = execCommandInPod(
		t, headPod.Name, headPod.Spec.Containers[0].Name, []string{"python3", remotePath})
	require.NoError(t, err, "Distributed JAX check failed (stdout=%q, stderr=%q)", stdout, stderr)
	t.Logf("JAX check output:\n%s", stdout)
	assert.Contains(t, stdout, "DYNAMIC_SLICING_JAX_CHECK_PASSED")
}

// TestDynamicSlicing_RayClusterValidation tests admission control interactions between
// kuberay-tpu-webhook and kueue-slice-controller for dynamic slicing RayClusters.
func TestDynamicSlicing_RayClusterValidation(t *testing.T) {
	ensureLocalQueue(t)

	t.Run("RejectsMissingParentTopologyWithoutKueueOrBypass", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-missing-parent"
		cluster.Namespace = testNamespace
		// Remove Kueue queue label so kueue-slice-controller does not inject TAS annotations.
		delete(cluster.Labels, kueueQueueLabel)

		assertRayClusterRejected(t, cluster,
			`must specify parent topology "cloud.google.com/gke-tpu-topology" in nodeSelector when using subslice annotation`)
	})

	t.Run("RejectsNumOfHostsMismatchWithSkipCheck", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-mismatch-skip"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Annotations = map[string]string{
			skipTPUWebhookCheckAnnotation: "true",
		}
		// 2x2x2 has 8 chips / 4 chips per host = 2 hosts, so 4 hosts must be rejected by kuberay-tpu-webhook.
		cluster.Spec.WorkerGroupSpecs[0].NumOfHosts = 4

		assertRayClusterRejected(t, cluster, errTopologyMismatch)
	})

	t.Run("RejectsNumOfHostsMismatchWithKueue", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-mismatch-kueue"
		cluster.Namespace = testNamespace
		cluster.Labels = applyQueueOverride(cluster.Labels)
		// 2x2x2 has 8 chips, but 4 hosts * 4 TPUs = 16 TPUs.
		cluster.Spec.WorkerGroupSpecs[0].NumOfHosts = 4

		assertRayClusterRejected(t, cluster, errTopologyMismatch)
	})

	t.Run("AdmitsClusterLevelSkipAnnotation", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-cluster-skip"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Annotations = map[string]string{
			skipTPUWebhookCheckAnnotation: "true",
		}

		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		assert.NoError(t, err, "Expected cluster-level skip-tpu-webhook-check annotation to pass validation")
	})

	t.Run("AdmitsWorkerGroupUnconstrainedTopologyAnnotation", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-unconstrained"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Spec.WorkerGroupSpecs[0].Template.Annotations[kueuePodSetUnconstrainedTopology] = "true"

		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		assert.NoError(t, err, "Expected podset-unconstrained-topology annotation to pass validation")
	})

	t.Run("AdmitsWorkerGroupSkipAnnotation", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-wg-skip"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Spec.WorkerGroupSpecs[0].Template.Annotations[skipTPUWebhookCheckAnnotation] = "true"

		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		assert.NoError(t, err, "Expected worker-group skip-tpu-webhook-check annotation to pass validation")
	})

	t.Run("AdmitsWorkerGroupPreferredTopologyAnnotation", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-preferred-topo"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Spec.WorkerGroupSpecs[0].Template.Annotations[kueuePodSetPreferredTopology] = gceTopologyBlockLabel

		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		assert.NoError(t, err, "Expected podset-preferred-topology annotation to pass validation")
	})

	t.Run("AdmitsWorkerGroupSliceTopologyConstraintsAnnotation", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-slice-constraints"
		cluster.Namespace = testNamespace
		delete(cluster.Labels, kueueQueueLabel)
		cluster.Spec.WorkerGroupSpecs[0].Template.Annotations[kueuePodSetSliceRequiredTopologyConstraints] =
			`[{"topology":"cloud.google.com/gke-tpu-partition-2x2x2-id","size":2}]`

		unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		assert.NoError(t, err, "Expected podset-slice-required-topology-constraints annotation to pass validation")
	})

	t.Run("RejectsInvalidDynamicSliceTopologyWithKueue", func(t *testing.T) {
		cluster := loadManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
		cluster.Name = "tpu7x-ds-val-invalid-topo"
		cluster.Namespace = testNamespace
		cluster.Labels = applyQueueOverride(cluster.Labels)
		cluster.Spec.WorkerGroupSpecs[0].Template.Annotations[gkeSliceTopologyAnnotation] = "2x2x99"

		assertRayClusterRejected(t, cluster, "topology")
	})

	t.Run("RejectsRayJobWithoutShutdownAfterJobFinishes", func(t *testing.T) {
		rayJob := loadRayJobManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-jax-rayjob.yaml")
		rayJob.Name = "tpu7x-ds-val-no-shutdown"
		rayJob.Namespace = testNamespace
		rayJob.Labels = applyQueueOverride(rayJob.Labels)
		rayJob.Spec.ShutdownAfterJobFinishes = false

		unstructJobObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rayJob)
		require.NoError(t, err)
		_, err = dynamicClient.Resource(rayJobGVR).Namespace(testNamespace).
			Create(t.Context(), &unstructured.Unstructured{Object: unstructJobObj}, metav1.CreateOptions{
				DryRun: []string{metav1.DryRunAll},
			})
		require.Error(t, err, "Expected Kueue-managed RayJob with shutdownAfterJobFinishes=false to be rejected")
		assert.Contains(t, err.Error(), "shutdownAfterJobFinishes")
	})
}

// TestDynamicSlicing_SubSlice_2x2x2 tests a single 2x2x2 dynamic subslice end-to-end:
// kueue-slice-controller defaulting, Kueue Workload & Slice CR provisioning, kuberay-tpu-webhook
// pod mutation & affinity bypass, DNS resolution, distributed JAX computation, and pod churn.
func TestDynamicSlicing_SubSlice_2x2x2(t *testing.T) {
	createdCluster := createRayClusterFromManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-subslice-2x2x2.yaml")
	clusterName := createdCluster.Name

	t.Run("KueueTPUACDefaultingAndSliceProvisioning", func(t *testing.T) {
		require.Len(t, createdCluster.Spec.WorkerGroupSpecs, 1)
		wgTemplate := createdCluster.Spec.WorkerGroupSpecs[0].Template

		assert.Equal(t, gceTopologyBlockLabel, wgTemplate.Annotations[kueuePodSetRequiredTopology])
		assert.Equal(
			t, "cloud.google.com/gke-tpu-partition-2x2x2-id",
			wgTemplate.Annotations[kueuePodSetSliceRequiredTopology])
		assert.Equal(t, "2", wgTemplate.Annotations[kueuePodSetSliceSize])
		assertSliceHealthNodeAffinity(t, wgTemplate.Spec.Affinity, "cloud.google.com/gke-tpu-partition-2x2x2-state")

		wl := waitForWorkloadAdmitted(t, "RayCluster", clusterName, 8*time.Minute)

		// Verify Kueue populated PodIndexLabel = ray.io/host-index on the worker PodSet.
		podSets, found, err := unstructured.NestedSlice(wl.Object, "spec", "podSets")
		require.NoError(t, err)
		require.True(t, found)
		foundWorkerPodSet := false
		for _, ps := range podSets {
			psMap, ok := ps.(map[string]any)
			if !ok || psMap["name"] != "tpu-worker-group" {
				continue
			}
			foundWorkerPodSet = true
			podIndexLabel, _, _ := unstructured.NestedString(psMap, "topologyRequest", "podIndexLabel")
			assert.Equal(t, utils.RayHostIndexKey, podIndexLabel, "Expected Kueue to set PodIndexLabel=ray.io/host-index")
		}
		assert.True(t, foundWorkerPodSet, "tpu-worker-group PodSet not found in Workload")

		slices := waitForSlicesActive(t, wl.GetName(), 1, 5*time.Minute)
		require.Len(t, slices, 1)
		sliceTopology, _, _ := unstructured.NestedString(slices[0].Object, "spec", "topology")
		sliceType, _, _ := unstructured.NestedString(slices[0].Object, "spec", "type")
		partitionIDs, _, _ := unstructured.NestedStringSlice(slices[0].Object, "spec", "partitionIds")
		assert.Equal(t, "2x2x2", sliceTopology)
		assert.Equal(t, "tpu7x", sliceType)
		assert.Len(t, partitionIDs, 1, "Expected 1 partitionId for 2x2x2 subslice")

		sliceLabels := slices[0].GetLabels()
		sliceAnnotations := slices[0].GetAnnotations()
		assert.Equal(t, testNamespace, sliceLabels[sliceUsedByNamespaceLabel])
		assert.NotEmpty(t, sliceLabels[sliceUsedByWorkloadLabel])
		assert.Equal(t, "tpu-worker-group", sliceAnnotations[sliceOwnerPodSetNameAnnotation])

		// Verify preferred nodeAffinity for cloud.google.com/gke-tpu-topology In [2x2x2] was
		// injected by kueue-slice-controller.
		require.NotNil(t, wgTemplate.Spec.Affinity)
		require.NotNil(t, wgTemplate.Spec.Affinity.NodeAffinity)
		foundPreferred := false
		for _, pref := range wgTemplate.Spec.Affinity.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			for _, expr := range pref.Preference.MatchExpressions {
				if expr.Key == gkeTPUTopologyLabel {
					foundPreferred = true
					assert.ElementsMatch(t, []string{"2x2x2"}, expr.Values)
				}
			}
		}
		assert.True(t, foundPreferred, "Expected preferred nodeAffinity for cloud.google.com/gke-tpu-topology=2x2x2")
	})
	if t.Failed() {
		t.FailNow()
	}

	var headPod *corev1.Pod
	t.Run("WebhookPodMutationAndAffinityBypass", func(t *testing.T) {
		headPod = waitForAllPodsRunning(t, clusterName, 2, 6*time.Minute)

		labelSelector := getLabelSelector(t, clusterName)
		pods := waitForPods(t, labelSelector, 3)

		workerIDs := make(map[string]bool)
		partitionIDs := make(map[string]bool)
		workerCount := 0

		for _, pod := range pods.Items {
			if pod.Labels[utils.RayNodeTypeLabelKey] != rayNodeTypeWorker {
				continue
			}
			workerCount++
			assertNoSyntheticPodAffinity(t, &pod)
			assertSliceHealthNodeAffinity(t, pod.Spec.Affinity, "cloud.google.com/gke-tpu-partition-2x2x2-state")

			envVars := pod.Spec.Containers[0].Env
			assertCommonTPUEnvVars(t, envVars)
			assertTorchTpuEnvsForV7x(t, envVars, "2,2,2,2", 2, 4)

			wID := envVarValue(envVars, tpuWorkerIDEnv)
			assert.Equal(t, pod.Labels[utils.RayHostIndexKey], wID,
				"TPU_WORKER_ID must match ray.io/host-index label")
			workerIDs[wID] = true

			assert.Equal(t, "tpu-worker-group-0", envVarValue(envVars, tpuNameEnv))
			assert.Equal(t, "8471", envVarValue(envVars, tpuProcessPortEnv))
			assert.NotEmpty(t, pod.Spec.Hostname)
			assert.Equal(t, fmt.Sprintf("%s-headless", clusterName), pod.Spec.Subdomain)

			expectedHostnames := buildExpectedHostnames(2, "tpu-worker-group-0", clusterName)
			assert.Equal(t, expectedHostnames, envVarValue(envVars, tpuWorkerHostnamesEnv))

			expectedProcessAddrs := buildExpectedProcessAddresses(2, "tpu-worker-group-0", clusterName, 1)
			assert.Equal(t, expectedProcessAddrs, envVarValue(envVars, tpuProcessAddressesEnv))

			// Verify both worker pods landed on nodes sharing the exact same 2x2x2 partition ID.
			require.NotEmpty(t, pod.Spec.NodeName)
			node, err := clientset.CoreV1().Nodes().Get(t.Context(), pod.Spec.NodeName, metav1.GetOptions{})
			require.NoError(t, err)
			partID := node.Labels["cloud.google.com/gke-tpu-partition-2x2x2-id"]
			assert.NotEmpty(t, partID, "Node %s missing 2x2x2 partition ID label", node.Name)
			partitionIDs[partID] = true
		}

		assert.Equal(t, 2, workerCount)
		assert.True(t, workerIDs["0"] && workerIDs["1"], "Expected TPU_WORKER_ID 0 and 1, got %v", workerIDs)
		assert.Len(t, partitionIDs, 1, "Both worker pods must be placed in the same 2x2x2 partition, got %v", partitionIDs)
	})
	if t.Failed() {
		t.FailNow()
	}

	t.Run("DNSResolution", func(t *testing.T) {
		labelSelector := getLabelSelector(t, clusterName)
		pods := waitForPods(t, labelSelector, 3)

		var workerPod *corev1.Pod
		for i := range pods.Items {
			if pods.Items[i].Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
				workerPod = &pods.Items[i]
				break
			}
		}
		require.NotNil(t, workerPod)

		hostnames := strings.Split(envVarValue(workerPod.Spec.Containers[0].Env, tpuWorkerHostnamesEnv), ",")
		require.Len(t, hostnames, 2)

		for _, hn := range hostnames {
			err := wait.PollUntilContextTimeout(
				t.Context(), 2*time.Second, 30*time.Second, true,
				func(ctx context.Context) (bool, error) {
					stdout, stderr, err := execCommandInPod(
						t, workerPod.Name, workerPod.Spec.Containers[0].Name, []string{"getent", "hosts", hn})
					if err != nil {
						t.Logf("Waiting for DNS %s (stderr=%q)...", hn, stderr)
						return false, nil
					}
					t.Logf("Resolved %s -> %s", hn, strings.TrimSpace(stdout))
					return true, nil
				})
			assert.NoError(t, err, "Failed resolving %s inside pod %s", hn, workerPod.Name)
		}
	})

	t.Run("DistributedJAXExecution", func(t *testing.T) {
		require.NotNil(t, headPod, "Head pod must be running before JAX check")
		runDistributedJAXCheck(t, headPod, 2, 4, 8, 16)
	})

	t.Run("PodChurnAndRecreation", func(t *testing.T) {
		labelSelector := getLabelSelector(t, clusterName)
		initialPods := waitForPods(t, labelSelector, 3)

		initialPodNames := make(map[string]bool)
		var workerPods []corev1.Pod
		for _, p := range initialPods.Items {
			initialPodNames[p.Name] = true
			if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
				workerPods = append(workerPods, p)
			}
		}
		require.Len(t, workerPods, 2)

		t.Logf("Deleting both worker pods (%s, %s) to test pod churn on an active dynamic slice...",
			workerPods[0].Name, workerPods[1].Name)
		deletePodsConcurrently(t, workerPods)

		triggerRayClusterReconcile(t, clusterName)
		recreatedPods := waitForRecreatedPods(t, labelSelector, 2, initialPodNames)

		recreatedIDs := make(map[string]bool)
		for _, p := range recreatedPods {
			assertNoSyntheticPodAffinity(t, &p)
			wID := envVarValue(p.Spec.Containers[0].Env, tpuWorkerIDEnv)
			assert.Equal(t, p.Labels[utils.RayHostIndexKey], wID)
			assertTorchTpuEnvsForV7x(t, p.Spec.Containers[0].Env, "2,2,2,2", 2, 4)
			recreatedIDs[wID] = true
		}
		assert.True(t, recreatedIDs["0"] && recreatedIDs["1"],
			"Recreated worker pods must receive TPU_WORKER_ID 0 and 1, got %v", recreatedIDs)

		waitForAllPodsRunning(t, clusterName, 2, 4*time.Minute)
	})
}

// TestDynamicSlicing_MultiSlice_2x2x2 tests multi-replica / Megascale multi-slice dynamic slicing
// (2 replicas of 2x2x2 = 4 hosts across 2 Slice CRs).
func TestDynamicSlicing_MultiSlice_2x2x2(t *testing.T) {
	createdCluster := createRayClusterFromManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-multislice-2x2x2.yaml")
	clusterName := createdCluster.Name

	wl := waitForWorkloadAdmitted(t, "RayCluster", clusterName, 8*time.Minute)

	// Verify Kueue PR #15677 hierarchical TAS fields: SubGroupCount=2, PodIndexLabel=ray.io/host-index,
	// SubGroupIndexLabel=ray.io/worker-group-replica-index.
	podSets, found, err := unstructured.NestedSlice(wl.Object, "spec", "podSets")
	require.NoError(t, err)
	require.True(t, found)
	for _, ps := range podSets {
		psMap, ok := ps.(map[string]any)
		if !ok || psMap["name"] != "tpu-worker-group" {
			continue
		}
		subGroupCount, _, _ := unstructured.NestedInt64(psMap, "topologyRequest", "subGroupCount")
		podIndexLabel, _, _ := unstructured.NestedString(psMap, "topologyRequest", "podIndexLabel")
		subGroupIndexLabel, _, _ := unstructured.NestedString(psMap, "topologyRequest", "subGroupIndexLabel")
		assert.Equal(t, int64(2), subGroupCount, "Expected SubGroupCount=2 for 2-replica worker group")
		assert.Equal(t, utils.RayHostIndexKey, podIndexLabel)
		assert.Equal(t, utils.RayWorkerReplicaIndexKey, subGroupIndexLabel)
	}

	// Verify 2 distinct Slice CRs are created on 2 distinct 2x2x2 partitions.
	slices := waitForSlicesActive(t, wl.GetName(), 2, 5*time.Minute)
	require.Len(t, slices, 2)
	slicePartitions := make(map[string]bool)
	for _, s := range slices {
		pIDs, _, _ := unstructured.NestedStringSlice(s.Object, "spec", "partitionIds")
		require.Len(t, pIDs, 1)
		slicePartitions[pIDs[0]] = true
	}
	assert.Len(t, slicePartitions, 2, "Expected the 2 Slice CRs to use 2 distinct 2x2x2 partitions")

	waitForAllPodsRunning(t, clusterName, 4, 6*time.Minute)
	pods := waitForPods(t, getLabelSelector(t, clusterName), 5)

	seenPairs := make(map[string]bool)
	partitionByReplica := make(map[string]string)

	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] != rayNodeTypeWorker {
			continue
		}
		assertNoSyntheticPodAffinity(t, &pod)

		envVars := pod.Spec.Containers[0].Env
		sliceID := envVarValue(envVars, megascaleSliceIDEnv)
		workerID := envVarValue(envVars, tpuWorkerIDEnv)
		coordAddr := envVarValue(envVars, megascaleCoordinatorAddressEnv)
		port := envVarValue(envVars, megascalePortEnv)

		assert.Equal(t, pod.Labels[utils.RayWorkerReplicaIndexKey], sliceID,
			"MEGASCALE_SLICE_ID must match ray.io/worker-group-replica-index")
		assert.Equal(t, pod.Labels[utils.RayHostIndexKey], workerID,
			"TPU_WORKER_ID must match ray.io/host-index")
		assert.Equal(t, fmt.Sprintf("tpu-worker-group-0-0.%s-headless:8081", clusterName), coordAddr)
		assert.Equal(t, "8081", port)
		assertTorchTpuEnvsForV7x(t, envVars, "2,2,2,2", 2, 4)

		pairKey := fmt.Sprintf("%s:%s", sliceID, workerID)
		seenPairs[pairKey] = true

		node, err := clientset.CoreV1().Nodes().Get(t.Context(), pod.Spec.NodeName, metav1.GetOptions{})
		require.NoError(t, err)
		partID := node.Labels["cloud.google.com/gke-tpu-partition-2x2x2-id"]
		if prev, exists := partitionByReplica[sliceID]; exists {
			assert.Equal(t, prev, partID, "All pods in replica %s must share the same 2x2x2 partition", sliceID)
		} else {
			partitionByReplica[sliceID] = partID
		}
	}

	assert.ElementsMatch(t, []string{"0:0", "0:1", "1:0", "1:1"}, mapKeys(seenPairs),
		"Expected unique (MEGASCALE_SLICE_ID, TPU_WORKER_ID) pairs across all 4 pods")
	assert.NotEqual(t, partitionByReplica["0"], partitionByReplica["1"],
		"Replica 0 and Replica 1 must be scheduled on distinct 2x2x2 partitions")
}

// TestDynamicSlicing_MultiWorkerGroup tests a RayCluster with two distinct TPU dynamic-slice
// worker groups (2x2x2 + 2x2x4) and one CPU worker group.
func TestDynamicSlicing_MultiWorkerGroup(t *testing.T) {
	createdCluster := createRayClusterFromManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-multi-group.yaml")
	clusterName := createdCluster.Name

	// Verify kueue-slice-controller mutated both TPU groups and skipped the CPU group.
	require.Len(t, createdCluster.Spec.WorkerGroupSpecs, 3)
	for _, wg := range createdCluster.Spec.WorkerGroupSpecs {
		switch wg.GroupName {
		case "tpu-group-a":
			assert.Equal(
				t, "cloud.google.com/gke-tpu-partition-2x2x2-id",
				wg.Template.Annotations[kueuePodSetSliceRequiredTopology])
			assert.Equal(t, "2", wg.Template.Annotations[kueuePodSetSliceSize])
		case "tpu-group-b":
			assert.Equal(
				t, "cloud.google.com/gke-tpu-partition-2x2x4-id",
				wg.Template.Annotations[kueuePodSetSliceRequiredTopology])
			assert.Equal(t, "4", wg.Template.Annotations[kueuePodSetSliceSize])
		case "cpu-group":
			assert.Empty(t, wg.Template.Annotations[kueuePodSetSliceRequiredTopology],
				"CPU worker group should not receive TAS slice annotations")
		}
	}

	wl := waitForWorkloadAdmitted(t, "RayCluster", clusterName, 8*time.Minute)
	slices := waitForSlicesActive(t, wl.GetName(), 2, 5*time.Minute)
	sliceTopologies := make(map[string]bool)
	for _, s := range slices {
		topo, _, _ := unstructured.NestedString(s.Object, "spec", "topology")
		sliceTopologies[topo] = true
	}
	assert.True(t, sliceTopologies["2x2x2"] && sliceTopologies["2x2x4"],
		"Expected one 2x2x2 Slice and one 2x2x4 Slice, got %v", sliceTopologies)

	// 2 workers in tpu-group-a + 4 workers in tpu-group-b + 1 worker in cpu-group = 7 workers.
	waitForAllPodsRunning(t, clusterName, 7, 6*time.Minute)
	pods := waitForPods(t, getLabelSelector(t, clusterName), 8)

	groupCounts := make(map[string]int)
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] != rayNodeTypeWorker {
			continue
		}
		group := pod.Labels[utils.RayNodeGroupLabelKey]
		groupCounts[group]++

		switch group {
		case "tpu-group-a":
			assertNoSyntheticPodAffinity(t, &pod)
			assertTorchTpuEnvsForV7x(t, pod.Spec.Containers[0].Env, "2,2,2,2", 2, 4)
		case "tpu-group-b":
			assertNoSyntheticPodAffinity(t, &pod)
			assertTorchTpuEnvsForV7x(t, pod.Spec.Containers[0].Env, "2,2,4,2", 4, 4)
		case "cpu-group":
			assert.False(t, hasEnvVar(pod.Spec.Containers[0].Env, tpuWorkerIDEnv),
				"CPU worker pod should not be mutated with TPU_WORKER_ID")
			assert.Empty(t, pod.Spec.Hostname, "CPU worker pod should not have hostname injected")
			assert.Empty(t, pod.Spec.Subdomain, "CPU worker pod should not have subdomain injected")
		}
	}

	assert.Equal(t, 2, groupCounts["tpu-group-a"])
	assert.Equal(t, 4, groupCounts["tpu-group-b"])
	assert.Equal(t, 1, groupCounts["cpu-group"])
}

// TestDynamicSlicing_MultiContainer_2x2x2 tests NUMA-aligned multi-container TPU7x pods
// (2 containers requesting 2 TPUs each per host) on a 2x2x2 dynamic slice.
func TestDynamicSlicing_MultiContainer_2x2x2(t *testing.T) {
	createdCluster := createRayClusterFromManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-multi-container.yaml")
	clusterName := createdCluster.Name

	wl := waitForWorkloadAdmitted(t, "RayCluster", clusterName, 8*time.Minute)
	waitForSlicesActive(t, wl.GetName(), 1, 5*time.Minute)
	waitForAllPodsRunning(t, clusterName, 2, 6*time.Minute)

	pods := waitForPods(t, getLabelSelector(t, clusterName), 3)
	seenContainerWorkerIDs := make(map[string]bool)
	expectedProcessAddrs := buildExpectedProcessAddresses(2, "tpu-worker-group-0", clusterName, 2)

	workerPodCount := 0
	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] != rayNodeTypeWorker {
			continue
		}
		workerPodCount++
		assertNoSyntheticPodAffinity(t, &pod)
		require.Len(t, pod.Spec.Containers, 2, "Expected 2 TPU containers per worker pod")

		hostIdx := pod.Labels[utils.RayHostIndexKey]
		var podWorkerIDs []string
		for cIdx, c := range pod.Spec.Containers {
			wID := envVarValue(c.Env, tpuWorkerIDEnv)
			port := envVarValue(c.Env, tpuProcessPortEnv)
			procAddrs := envVarValue(c.Env, tpuProcessAddressesEnv)

			assert.Equal(t, fmt.Sprint(8471+cIdx), port)
			assert.Equal(t, expectedProcessAddrs, procAddrs)
			assert.NotEmpty(t, wID, "TPU_WORKER_ID should not be empty")
			seenContainerWorkerIDs[wID] = true
			podWorkerIDs = append(podWorkerIDs, wID)
		}
		require.Len(t, podWorkerIDs, 2)
		assert.NotEqual(t, podWorkerIDs[0], podWorkerIDs[1],
			"Host %s container worker IDs must be distinct", hostIdx)
	}

	assert.Equal(t, 2, workerPodCount)
	assert.ElementsMatch(t, []string{"0", "1", "2", "3"}, mapKeys(seenContainerWorkerIDs),
		"Expected container TPU_WORKER_IDs 0, 1, 2, 3 across the 2 hosts")
}

// TestDynamicSlicing_SuperSlice_4x4x8 tests a 32-host (128-chip) 4x4x8 SuperSlice spanning
// two 16-host sub-blocks / nodepools. This exercises two-level TAS (podset-slice-size=16)
// and proves that kuberay-tpu-webhook's affinity bypass allows pods of one replica to span
// multiple GKE nodepools.
func TestDynamicSlicing_SuperSlice_4x4x8(t *testing.T) {
	createdCluster := createRayClusterFromManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-superslice-4x4x8.yaml")
	clusterName := createdCluster.Name

	require.Len(t, createdCluster.Spec.WorkerGroupSpecs, 1)
	wgTemplate := createdCluster.Spec.WorkerGroupSpecs[0].Template
	assert.Equal(t, gceTopologyBlockLabel, wgTemplate.Annotations[kueuePodSetRequiredTopology])
	assert.Equal(t, gkePartition4x4x4IDLabel, wgTemplate.Annotations[kueuePodSetSliceRequiredTopology])
	assert.Equal(t, "16", wgTemplate.Annotations[kueuePodSetSliceSize],
		"4x4x8 superslice (32 hosts / 2 sub-blocks) must set podset-slice-size=16")

	wl := waitForWorkloadAdmitted(t, "RayCluster", clusterName, 10*time.Minute)
	slices := waitForSlicesActive(t, wl.GetName(), 1, 6*time.Minute)
	require.Len(t, slices, 1)
	partitionIDs, _, _ := unstructured.NestedStringSlice(slices[0].Object, "spec", "partitionIds")
	assert.Len(t, partitionIDs, 2, "Expected 4x4x8 SuperSlice CR to contain 2 4x4x4 partitionIds")

	waitForAllPodsRunning(t, clusterName, 32, 8*time.Minute)
	pods := waitForPods(t, getLabelSelector(t, clusterName), 33)

	workerIDs := make(map[string]bool)
	nodePools := make(map[string]int)
	subBlocks := make(map[string]int)

	for _, pod := range pods.Items {
		if pod.Labels[utils.RayNodeTypeLabelKey] != rayNodeTypeWorker {
			continue
		}
		assertNoSyntheticPodAffinity(t, &pod)
		envVars := pod.Spec.Containers[0].Env
		assertTorchTpuEnvsForV7x(t, envVars, "4,4,8,2", 32, 4)

		wID := envVarValue(envVars, tpuWorkerIDEnv)
		assert.Equal(t, pod.Labels[utils.RayHostIndexKey], wID)
		workerIDs[wID] = true

		node, err := clientset.CoreV1().Nodes().Get(t.Context(), pod.Spec.NodeName, metav1.GetOptions{})
		require.NoError(t, err)
		nodePools[node.Labels[gkeNodePoolLabel]]++
		subBlocks[node.Labels[gkePartition4x4x4IDLabel]]++
	}

	assert.Len(t, workerIDs, 32, "Expected 32 unique TPU_WORKER_IDs (0..31)")
	assert.Len(t, subBlocks, 2, "Expected 4x4x8 SuperSlice pods to span 2 4x4x4 sub-blocks, got %v", subBlocks)
	assert.GreaterOrEqual(t, len(nodePools), 2,
		"Expected 4x4x8 SuperSlice pods to span 2 nodepools without synthetic "+
			"gke-nodepool podAffinity blocking them, got %v", nodePools)
}

// TestDynamicSlicing_RayJob_2x2x2 tests a Kueue-managed RayJob using TPU7x Dynamic Slicing
// (2x2x2 subslice) running Ray Train + JAX to SUCCEEDED and releasing its Slice CR.
func TestDynamicSlicing_RayJob_2x2x2(t *testing.T) {
	ensureLocalQueue(t)

	// 1. Ensure the ConfigMap containing jax_trainer.py exists in testNamespace.
	scriptBytes, err := os.ReadFile("../scripts/jax_trainer.py")
	require.NoError(t, err, "Failed reading ../scripts/jax_trainer.py")

	cmName := "rayjob-ds-script"
	_ = clientset.CoreV1().ConfigMaps(testNamespace).Delete(t.Context(), cmName, metav1.DeleteOptions{})
	_, err = clientset.CoreV1().ConfigMaps(testNamespace).Create(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: testNamespace,
		},
		Data: map[string]string{
			"jax_trainer.py": string(scriptBytes),
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err, "Failed creating ConfigMap %s", cmName)
	t.Cleanup(func() {
		_ = clientset.CoreV1().ConfigMaps(testNamespace).Delete(context.Background(), cmName, metav1.DeleteOptions{})
	})

	// 2. Load and create the RayJob manifest.
	rayJob := loadRayJobManifest(t, "../manifests/dynamic-slicing/tpu7x-ds-jax-rayjob.yaml")
	rayJob.Namespace = testNamespace
	rayJob.Labels = applyQueueOverride(rayJob.Labels)
	if img := os.Getenv("RAY_TPU7X_IMAGE"); img != "" {
		for i := range rayJob.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec.Containers {
			rayJob.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec.Containers[i].Image = img
		}
		for i := range rayJob.Spec.RayClusterSpec.WorkerGroupSpecs {
			for j := range rayJob.Spec.RayClusterSpec.WorkerGroupSpecs[i].Template.Spec.Containers {
				rayJob.Spec.RayClusterSpec.WorkerGroupSpecs[i].Template.Spec.Containers[j].Image = img
			}
		}
	}

	_ = dynamicClient.Resource(rayJobGVR).Namespace(testNamespace).
		Delete(t.Context(), rayJob.Name, metav1.DeleteOptions{})
	waitForClusterCleanup(t, "RayJob", rayJob.Name)

	unstructJobObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(rayJob)
	require.NoError(t, err)

	createdUnstructJob, err := dynamicClient.Resource(rayJobGVR).Namespace(testNamespace).
		Create(t.Context(), &unstructured.Unstructured{Object: unstructJobObj}, metav1.CreateOptions{})
	require.NoError(t, err, "Failed creating RayJob %s", rayJob.Name)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		_ = dynamicClient.Resource(rayJobGVR).Namespace(testNamespace).
			Delete(cleanupCtx, rayJob.Name, metav1.DeleteOptions{})
		waitForClusterCleanupWithContext(cleanupCtx, t, "RayJob", rayJob.Name)
	})

	// 3. Verify kueue-slice-controller mutated RayJob.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template.
	var createdJob rayv1.RayJob
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(createdUnstructJob.Object, &createdJob)
	require.NoError(t, err)
	require.NotNil(t, createdJob.Spec.RayClusterSpec)
	require.Len(t, createdJob.Spec.RayClusterSpec.WorkerGroupSpecs, 1)

	wgTemplate := createdJob.Spec.RayClusterSpec.WorkerGroupSpecs[0].Template
	assert.Equal(t, gceTopologyBlockLabel, wgTemplate.Annotations[kueuePodSetRequiredTopology])
	assert.Equal(
		t, "cloud.google.com/gke-tpu-partition-2x2x2-id",
		wgTemplate.Annotations[kueuePodSetSliceRequiredTopology])
	assert.Equal(t, "2", wgTemplate.Annotations[kueuePodSetSliceSize])
	assertSliceHealthNodeAffinity(t, wgTemplate.Spec.Affinity, "cloud.google.com/gke-tpu-partition-2x2x2-state")

	// 4. Wait for Workload admission and Slice CR activation.
	wl := waitForWorkloadAdmitted(t, "RayJob", rayJob.Name, 8*time.Minute)
	slices := waitForSlicesActive(t, wl.GetName(), 1, 5*time.Minute)
	require.Len(t, slices, 1)
	sliceName := slices[0].GetName()

	// 5. Wait for RayJob to reach SUCCEEDED.
	waitForRayJobSuccess(t, rayJob.Name, 15*time.Minute)

	// 6. Verify Slice CR is released (KueueAcInUse=False) or deleted after job completion.
	err = wait.PollUntilContextTimeout(
		t.Context(), 5*time.Second, 3*time.Minute, true,
		func(ctx context.Context) (bool, error) {
			s, err := dynamicClient.Resource(sliceGVR).Get(ctx, sliceName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			if err != nil {
				return false, err
			}
			conds, _, _ := unstructured.NestedSlice(s.Object, "status", "conditions")
			for _, c := range conds {
				cm, ok := c.(map[string]any)
				if !ok {
					continue
				}
				if cm["type"] == sliceInUseConditionType && cm["status"] == "False" {
					t.Logf("Slice %s released after RayJob completion (KueueAcInUse=False)", sliceName)
					return true, nil
				}
			}
			return false, nil
		})
	assert.NoError(t, err, "Expected Slice %s to be deleted or marked KueueAcInUse=False after RayJob finished", sliceName)
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
