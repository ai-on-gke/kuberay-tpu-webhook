# KubeRay with TPUs User Guide

This page contains instructions for how to set up Ray on GKE with TPUs.

## Prerequisites

Please follow the official [Google Cloud documentation](https://cloud.google.com/tpu/docs/tpus-in-gke) for an introduction to TPUs. In particular, please ensure that your GCP project has sufficient quotas to provision the cluster, see [this link](https://cloud.google.com/tpu/docs/tpus-in-gke#ensure-quotas) for details.

For additional useful information about TPUs on GKE (such as topology configurations and availability), see [this page](https://cloud.google.com/kubernetes-engine/docs/concepts/tpus).

In addition, please ensure the following are installed on your local development environment:

* Helm (v3.9.3+)
* Kubectl

## Version Compatibility & Recommendations

> **Recommendation:** Always install the latest version of the webhook via the published Helm chart.
>
> All webhook versions are **strictly backwards-compatible** with earlier TPU generations from v4 through tpu7x and earlier KubeRay Operator versions 1.1.1+. For optimal stability and out-of-the-box native worker replica indexing, **KubeRay v1.6.0+** is recommended. Upgrading the webhook does not require an upgrade of your KubeRay operator or Ray application code.

### Compatibility Matrix

| Webhook Version | Minimum KubeRay Version | Supported TPU Generations | Key Features Introduced |
|:---|:---|:---|:---|
| **`1.4.0`** | **1.1.1+** | TPU v4, v5e, v5p, v6e, tpu7x (Ironwood) | PyTorch / TorchXLA TPU support, TPU subslicing, and dynamic TLS certificate reloading. |
| **`1.3.1` – `1.3.6`** | **1.1.1+** | TPU v4, v5e, v5p, v6e, tpu7x (Ironwood) | TPU tpu7x (Ironwood) support and NUMA multi-container workloads. |
| **`1.3.0`** | **1.1.1+** | TPU v4, v5e, v5p, v6e | Megascale multi-slice TPU training support. |
| **`1.2.4` – `1.2.6`** | **1.1.1+** | TPU v4, v5e, v5p, v6e | Single-host and multi-host TPU support for JAX and libtpu, and TPU metrics routing for Ray Dashboard. |

## Container Images

Pre-built container images are hosted at
[us-docker.pkg.dev/ai-on-gke/kuberay-tpu-webhook/tpu-webhook][1] and have a `-gke.X` suffix.

## Install the KubeRay TPU Webhook

The KubeRay TPU Webhook automatically bootstraps the TPU environment for TPU clusters. The webhook needs to be installed once per GKE cluster and requires a KubeRay Operator running v1.1+ and GKE cluster version of 1.28+. The webhook requires [cert-manager](https://github.com/cert-manager/cert-manager) to be installed in-cluster to handle TLS certificate injection. cert-manager can be installed in both GKE standard and autopilot clusters using the following helm commands:

```shell
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install --create-namespace --namespace cert-manager --set installCRDs=true --set global.leaderElection.namespace=cert-manager cert-manager jetstack/cert-manager
```

After installing cert-manager, it may take up to two minutes for the certificate to become ready.

Ensure you are authenticated to use artifact registry:
```shell
gcloud auth login
gcloud auth configure-docker us-docker.pkg.dev
```

Installing the webhook:
```shell
helm install kuberay-tpu-webhook oci://us-docker.pkg.dev/ai-on-gke/kuberay-tpu-webhook-helm/kuberay-tpu-webhook
```

The above command can be edited with `-f` or `--set` flags to pass in a custom values file or key-value pair respectively for the chart (i.e. `--set tpuWebhook.image.tag=v1.4.0-gke.3`).

For common errors encountered when deploying the webhook, see the [Troubleshooting guide](https://github.com/ai-on-gke/kuberay-tpu-webhook/tree/main/Troubleshooting.md).

## What the Webhook Does Automatically

When you submit a RayCluster resource requesting TPUs, this mutating webhook intercepts the Pod creation and automatically injects the required configurations so that libtpu, JAX, PyTorch, and Ray can initialize correctly. You do not need to manually configure these in your manifests.

* **Network Initialization:**
    * **TPU v4 - v6e:** Automatically generates and injects the `TPU_WORKER_HOSTNAMES` list for multi-host networking. The webhook also sets the `subdomain` and `hostname` fields in the Pod spec.
    * **TPU tpu7x (Ironwood):** In addition to the vars and fields injected in previous versions, also automatically generates and injects the new `TPU_PROCESS_ADDRESSES` and `TPU_PROCESS_PORT` required for tpu7x architecture. `TPU_PROCESS_ADDRESSES` is identical to `TPU_WORKER_HOSTNAMES`, but with the container port appended for each address.
* **Worker Identification:** Calculates and injects `TPU_WORKER_ID` and `TPU_NAME` (a unique identifier for the replica group) for multi-host and multi-container coordination.
* **Multi-Container (NUMA) Support:** Natively supports tpu7x Pods that run multiple NUMA-aligned containers, assigning unique ports and IDs to each ML process. It's important to note that multi-node support per Pod with KubeRay is experimental.
* **Megascale (Multi-Slice) Support:** If `MEGASCALE_NUM_SLICES` is set explicitly in the Pod spec of your Ray container, the webhook automatically calculates and injects `MEGASCALE_SLICE_ID`, `MEGASCALE_COORDINATOR_ADDRESS`, and `MEGASCALE_PORT`. If utilizing the [JaxTrainer](https://docs.ray.io/en/latest/train/api/doc/ray.train.v2.jax.JaxTrainer.html#ray.train.v2.jax.JaxTrainer) in Ray Train, `MEGASCALE_NUM_SLICES` and related env vars are calculated for you based on the value of `num_workers`, `accelerator_type`, and `topology` and set automatically at runtime.
* **PyTorch / Torch TPU Support:** Automatically generates and injects `TORCH_TPU_TOPOLOGY` and `TORCH_TPU_SLICEBUILDER_ADDRESSES` for distributed PyTorch / TorchXLA TPU workloads.
* **TPU Subslicing Support:** If `cloud.google.com/gke-tpu-slice-topology` is set as an annotation on the Pod template, the webhook automatically calculates and injects the appropriate `podAffinity` (e.g., `cloud.google.com/gce-topology-subblock`) to ensure that all hosts in a subslice are co-located on the same physical grouping within a larger TPU slice. This enables running multiple independent smaller workloads on a single large TPU reservation.
* **Device Plugin Routing:** Injects `TPU_DEVICE_PLUGIN_HOST_IP` and `TPU_DEVICE_PLUGIN_ADDR` to ensure the container communicates with the correct node-level hardware plugin. These environment variables are utilized in Ray to scrape per-node metrics like Tensor Core utilization, HBM utilization, TPU duty cycle, and memory usage which are then viewable on the Ray Dashboard. See [View TPU metrics on the Ray Dashboard](https://docs.cloud.google.com/kubernetes-engine/docs/add-on/ray-on-gke/how-to/view-tpu-metrics).

## Validating Webhook Rules

In addition to automatically injecting environment variables, the webhook also acts as a validating admission controller. It analyzes your `RayCluster` custom resource upon submission and will reject the creation of the cluster if the configurations of your TPU worker groups are invalid.

The webhook evaluates each `workerGroupSpec` against the following rules:

* **Non-TPU Workloads are Ignored:** If a worker group's containers do not request `google.com/tpu` resources, the webhook immediately admits them without further checks.
* **Missing NumOfHosts:** If `numOfHosts` is set to `0` or omitted for a TPU multi-host worker group (determined from the topology and accelerator type), the cluster is rejected. `numOfHosts` defaults to `1` in KubeRay.
* **Missing Node Selectors:** If a TPU worker group is missing the `cloud.google.com/gke-tpu-topology` node selector the cluster is rejected.
* **Strict Topology Validation:** The webhook strictly enforces that the number of physical TPU hosts requested matches your requested physical topology. It calculates this using the following formula:
    * **Expected Hosts:** `max(Total Chips / Chips Per Host, 1)`
    * If the calculated `Expected Hosts` does not exactly match the `numOfHosts` defined in your `workerGroupSpec`, the cluster is rejected with the error: `"Number of workers in worker group not equal to specified topology"`.
  * **Example:** If your node selector specifies a `2x2x2` topology (8 total chips) and your container requests `4` TPUs (`google.com/tpu: "4"`), your `numOfHosts` must be set to `2`.
* **Subslice Ambiguity Validation:** If a subslice is requested via the `cloud.google.com/gke-tpu-slice-topology` annotation, the webhook verifies that the requested `numOfHosts` corresponds to a unique physical grouping (block, subblock, etc.) available in the targeted nodes. If the request is ambiguous or no grouping matches the host count, the cluster is rejected. Note that if there are no existing nodes in the targeted nodepool (such as during scale-up on GKE Autopilot), the RayCluster will be admitted with a warning to allow auto-provisioning/scale-up to trigger, but subslice scheduling and co-location guarantees will not work because the webhook cannot discover the node topology; in this case, the RayCluster must be re-created after the nodes are provisioned.

## Install the KubeRay TPU Webhook from Source

To install the KubeRay TPU webhook from source:

1. `git clone https://github.com/ai-on-gke/kuberay-tpu-webhook`
1. `cd kuberay-tpu-webhook`
1. `make deploy`
    1. this will create the webhook deployment, configs, and service in the "ray-system" namespace
    1. to change the namespace, edit the "namespace" value in each .yaml in deployments/ and certs/
1. `make deploy-cert`

## Creating the KubeRay Cluster

You can find sample TPU cluster manifests for [single-host](https://github.com/ray-project/kuberay/blob/master/ray-operator/config/samples/ray-cluster.tpu-v4-singlehost.yaml) and [multi-host](https://github.com/ray-project/kuberay/blob/master/ray-operator/config/samples/ray-cluster.tpu-v4-multihost.yaml) here.

For a quick-start guide to using TPUs with KubeRay, see [Use TPUs with KubeRay](https://docs.ray.io/en/latest/cluster/kubernetes/user-guides/tpu.html).

## Running Sample Workloads

1. Save the following to a local file (e.g. `test_tpu.py`):

```python
import ray

ray.init(
    runtime_env={
        "pip": [
            "jax[tpu]",
            "-f https://storage.googleapis.com/jax-releases/libtpu_releases.html",
        ]
    }
)


@ray.remote(resources={"TPU": 4})
def tpu_cores():
    import jax
    return "TPU cores:" + str(jax.device_count())

num_workers = 4
result = [tpu_cores.remote() for _ in range(num_workers)]
print(ray.get(result))
```

1. `kubectl port-forward svc/RAYCLUSTER-NAME-head-svc dashboard &` where `RAYCLUSTER-NAME` is the
   `.metadata.name` of the RayCluster in the cluster manifest you used.
1. `RAY_ADDRESS=http://localhost:8265 ray job submit --runtime-env-json='{"working_dir": "."}' -- python test_tpu.py`

For a more advanced workload running Stable Diffusion on TPUs, see [here](https://cloud.google.com/kubernetes-engine/docs/add-on/ray-on-gke/tutorials/deploy-ray-serve-stable-diffusion-tpu). For an example of serving a LLM with TPUs, RayServe, and KubeRay, see [here](https://cloud.google.com/kubernetes-engine/docs/tutorials/serve-lllm-tpu-ray).

## End-to-End Testing Guide

To run E2E validation, mutation tests, and dynamic pod churn qualification for this webhook on GKE, follow the developer instructions in the **[E2E Testing Guide](e2e/README.md)**.

[1]: https://console.cloud.google.com/artifacts/docker/ai-on-gke/us/kuberay-tpu-webhook/tpu-webhook
