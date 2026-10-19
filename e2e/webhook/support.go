//go:build e2e || dynamic_slicing

package webhook

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
)

var (
	kubeconfig    *string
	clientset     kubernetes.Interface
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
	testNamespace = "default"
)

const (
	// TPU webhook environment variable names.
	tpuWorkerIDEnv           = "TPU_WORKER_ID"
	tpuNameEnv               = "TPU_NAME"
	tpuDevicePluginHostIPEnv = "TPU_DEVICE_PLUGIN_HOST_IP"
	tpuDevicePluginAddrEnv   = "TPU_DEVICE_PLUGIN_ADDR"
	tpuWorkerHostnamesEnv    = "TPU_WORKER_HOSTNAMES"
	tpuProcessAddressesEnv   = "TPU_PROCESS_ADDRESSES"
	tpuProcessPortEnv        = "TPU_PROCESS_PORT"

	// Megascale environment variable names.
	megascaleSliceIDEnv            = "MEGASCALE_SLICE_ID"
	megascaleCoordinatorAddressEnv = "MEGASCALE_COORDINATOR_ADDRESS"
	megascalePortEnv               = "MEGASCALE_PORT"

	// GKE TPU-specific labels.
	replicaIndexLabelKey       = "ray.io/worker-group-replica-index"
	legacyReplicaIndexLabelKey = "replicaIndex"

	// Ray-specific labels.
	rayNodeTypeWorker = string(rayv1.WorkerNode)
	rayNodeTypeHead   = string(rayv1.HeadNode)

	// Validation error substrings.
	errTopologyMismatch = "Number of workers in worker group not equal to specified topology"
)

var (
	rayClusterGVR = schema.GroupVersionResource{
		Group:    "ray.io",
		Version:  "v1",
		Resource: "rayclusters",
	}
	rayJobGVR = schema.GroupVersionResource{
		Group:    "ray.io",
		Version:  "v1",
		Resource: "rayjobs",
	}
)

func init() {
	if flag.Lookup("kubeconfig") == nil {
		if home := os.Getenv("HOME"); home != "" {
			kubeconfig = flag.String("kubeconfig",
				filepath.Join(home, ".kube", "config"),
				"(optional) absolute path to the kubeconfig file")
		} else {
			kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
		}
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

func waitForPods(t *testing.T, labelSelector string, expectedCount int) *corev1.PodList {
	t.Helper()
	var pods *corev1.PodList
	err := wait.PollUntilContextTimeout(t.Context(), 1*time.Second, 30*time.Second,
		true, func(ctx context.Context) (bool, error) {
			var err error
			pods, err = clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				return false, err
			}
			if len(pods.Items) >= expectedCount {
				return true, nil
			}
			return false, nil
		})
	if err != nil {
		t.Fatalf("Error waiting for %d pods with selector %s: %v (found %d pods)",
			expectedCount, labelSelector, err, len(pods.Items))
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

func deletePodsConcurrently(t *testing.T, pods []corev1.Pod) {
	t.Helper()
	var wg sync.WaitGroup
	for _, pod := range pods {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			err := clientset.CoreV1().Pods(testNamespace).Delete(t.Context(), podName, metav1.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("Failed to delete pod %s: %v", podName, err)
				return
			}

			// Wait for the pod to be completely removed from the API server (graceful cleanup finished)
			err = wait.PollUntilContextTimeout(t.Context(), 2*time.Second, 60*time.Second,
				true, func(ctx context.Context) (bool, error) {
					_, err := clientset.CoreV1().Pods(testNamespace).Get(ctx, podName, metav1.GetOptions{})
					if err != nil {
						if apierrors.IsNotFound(err) {
							return true, nil
						}
						return false, err
					}
					return false, nil
				})
			if err != nil {
				t.Errorf("Pod %s was not fully deleted within 60s: %v", podName, err)
			}
		}(pod.Name)
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

func buildExpectedHostnames(numOfHosts int, replicaIndexLabelVal string, clusterName string) string {
	headlessService := fmt.Sprintf("%s-headless", clusterName)
	hostnames := make([]string, numOfHosts)
	for j := 0; j < numOfHosts; j++ {
		hostnames[j] = fmt.Sprintf("%s-%d.%s", replicaIndexLabelVal, j, headlessService)
	}
	return strings.Join(hostnames, ",")
}

func buildExpectedProcessAddresses(
	numOfHosts int, replicaIndexLabelVal string, clusterName string, numTpuContainers int,
) string {
	headlessService := fmt.Sprintf("%s-headless", clusterName)
	var addresses []string
	for h := 0; h < numOfHosts; h++ {
		hostName := fmt.Sprintf("%s-%d.%s", replicaIndexLabelVal, h, headlessService)
		for c := 0; c < numTpuContainers; c++ {
			port := 8471 + c
			addresses = append(addresses, fmt.Sprintf("%s:%d", hostName, port))
		}
	}
	return strings.Join(addresses, ",")
}

func execCommandInPod(t *testing.T, podName string, containerName string, cmd []string) (string, string, error) {
	t.Helper()
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(testNamespace).
		SubResource("exec")
	option := &corev1.PodExecOptions{
		Container: containerName,
		Command:   cmd,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
	req.VersionedParams(option, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(t.Context(), remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

func writeLocalFileToPod(t *testing.T, podName string, containerName string, localPath string, remotePath string) {
	t.Helper()
	content, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("Failed to read local file %s: %v", localPath, err)
	}
	t.Logf("Writing local file %s to %s in pod %s...", localPath, remotePath, podName)
	cmd := []string{"bash", "-c", "cat << 'EOF' > " + remotePath + "\n" + string(content) + "\nEOF\n"}
	stdout, stderr, err := execCommandInPod(t, podName, containerName, cmd)
	if err != nil {
		t.Fatalf("Failed to write file %s to pod: %v (stdout: %q, stderr: %q)", remotePath, err, stdout, stderr)
	}
}

func waitForAllPodsRunning(
	t *testing.T, clusterName string, expectedWorkerCount int, timeout time.Duration,
) *corev1.Pod {
	t.Helper()
	t.Logf("Waiting for head pod and all %d worker pods of cluster %s to reach Running phase...",
		expectedWorkerCount, clusterName)
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)
	var headPod *corev1.Pod
	err := wait.PollUntilContextTimeout(t.Context(), 5*time.Second, timeout,
		true, func(ctx context.Context) (bool, error) {
			pods, err := clientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				return false, err
			}

			headRunning := false
			workerRunningCount := 0
			for _, p := range pods.Items {
				if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeHead && p.Status.Phase == corev1.PodRunning {
					headRunning = true
					pCopy := p
					headPod = &pCopy
				} else if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker {
					if p.Status.Phase == corev1.PodRunning {
						containerRunning := false
						for _, cs := range p.Status.ContainerStatuses {
							if cs.Name == p.Spec.Containers[0].Name && cs.State.Running != nil {
								containerRunning = true
								break
							}
						}
						if containerRunning {
							workerRunningCount++
						}
					}
				}
			}
			t.Logf("Checking pod states... Head Running: %t, Workers Running: %d/%d",
				headRunning, workerRunningCount, expectedWorkerCount)
			return headRunning && workerRunningCount == expectedWorkerCount, nil
		})
	if err != nil {
		t.Fatalf("Pods of cluster %s failed to reach Running phase within %v: %v", clusterName, timeout, err)
	}
	return headPod
}

func triggerRayClusterReconcile(t *testing.T, clusterName string) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cluster, err := dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Get(t.Context(), clusterName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		annotations := cluster.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations["tpu-webhook.gke.io/reconcile-trigger"] = fmt.Sprint(time.Now().UnixNano())
		cluster.SetAnnotations(annotations)

		_, err = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Update(t.Context(), cluster, metav1.UpdateOptions{})
		return err
	})

	if err != nil {
		t.Fatalf("Failed to trigger reconciliation for RayCluster %s: %v", clusterName, err)
	}
	t.Logf("Successfully triggered immediate KubeRay reconciliation for %s", clusterName)
}

func getLabelSelector(t *testing.T, clusterName string) string {
	t.Helper()
	labelSelector := fmt.Sprintf("%s=%s", utils.RayClusterLabelKey, clusterName)
	t.Logf("Looking for pods with selector: %s", labelSelector)
	return labelSelector
}

func assertCommonTPUEnvVars(t *testing.T, envVars []corev1.EnvVar) {
	t.Helper()
	assert.True(t, hasEnvVar(envVars, tpuWorkerIDEnv), "Missing "+tpuWorkerIDEnv)
	assert.NotEmpty(t, envVarValue(envVars, tpuWorkerIDEnv), tpuWorkerIDEnv+" is empty")

	assert.True(t, hasEnvVar(envVars, tpuNameEnv), "Missing "+tpuNameEnv)
	assert.NotEmpty(t, envVarValue(envVars, tpuNameEnv), tpuNameEnv+" is empty")

	assert.True(t, hasEnvVar(envVars, tpuDevicePluginHostIPEnv), "Missing "+tpuDevicePluginHostIPEnv)
	assert.True(t, hasEnvVar(envVars, tpuDevicePluginAddrEnv), "Missing "+tpuDevicePluginAddrEnv)
}

func waitForRecreatedPods(
	t *testing.T, labelSelector string, expectedCount int, initialPodNames map[string]bool,
) []corev1.Pod {
	t.Helper()
	var recreatedPods []corev1.Pod
	err := wait.PollUntilContextTimeout(t.Context(), 3*time.Second, 90*time.Second,
		true, func(ctx context.Context) (bool, error) {
			currentPods, err := clientset.CoreV1().Pods(testNamespace).
				List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				return false, err
			}

			recreatedPods = nil
			for _, p := range currentPods.Items {
				if p.Labels[utils.RayNodeTypeLabelKey] == rayNodeTypeWorker && !initialPodNames[p.Name] {
					if hasEnvVar(p.Spec.Containers[0].Env, tpuWorkerIDEnv) {
						recreatedPods = append(recreatedPods, p)
					}
				}
			}
			if len(recreatedPods) >= expectedCount {
				return true, nil
			}
			t.Logf("Waiting for recreated pods... (found %d/%d)", len(recreatedPods), expectedCount)
			return false, nil
		})
	if err != nil {
		t.Fatalf("Timed out waiting for recreated pods: %v (found %d/%d)", err, len(recreatedPods), expectedCount)
	}
	return recreatedPods
}

// assertRayClusterRejected is a helper function to verify that creating a RayCluster
// with invalid configurations is rejected by the validating webhook.
func assertRayClusterRejected(t *testing.T, cluster *rayv1.RayCluster, expectedErrorMessage string) {
	t.Helper()
	require.NotEmpty(t, expectedErrorMessage, "expectedErrorMessage must not be empty to avoid false-positive test passes")

	unstructuredObj, err := k8sruntime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		t.Fatalf("Error converting RayCluster to unstructured: %v", err)
	}

	res, err := dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
		Create(t.Context(), &unstructured.Unstructured{Object: unstructuredObj}, metav1.CreateOptions{})
	if err == nil {
		t.Logf("Cleaning up leaked RayCluster %q...", res.GetName())
		_ = dynamicClient.Resource(rayClusterGVR).Namespace(testNamespace).
			Delete(t.Context(), res.GetName(), metav1.DeleteOptions{})
		t.Fatalf("Expected cluster creation to fail due to webhook validation, but it succeeded")
	}

	statusErr, ok := err.(*apierrors.StatusError)
	if !ok {
		t.Fatalf("Expected StatusError from API server, got: %T (error: %v)", err, err)
	}

	assert.Contains(
		t,
		statusErr.Error(),
		expectedErrorMessage,
		"Validation rejected, but with unexpected error message",
	)
}

// waitForRayJobSuccess is a helper function to poll and block until the RayJob
// successfully completes (SUCCEEDED).
func waitForRayJobSuccess(t *testing.T, jobName string, timeout time.Duration) {
	t.Helper()
	t.Logf("Waiting for RayJob %s to reach SUCCEEDED status...", jobName)

	err := wait.PollUntilContextTimeout(t.Context(), 10*time.Second, timeout,
		true, func(ctx context.Context) (bool, error) {
			unstructJob, err := dynamicClient.Resource(rayJobGVR).
				Namespace(testNamespace).Get(ctx, jobName, metav1.GetOptions{})
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
