# E2E Mutation & Validation Tests for KubeRay TPU Webhook

This directory contains end-to-end (E2E) qualification, mutation, and validation tests to ensure that the KubeRay TPU webhook correctly injects TPU-specific environment variables, labels, hostnames, and enforces topology validations on GKE TPU workloads.

---

## Prerequisites

1.  **Tools**: Install `gcloud`, `kubectl`, and Go `1.26+`.
2.  **GCP Project & Quotas**: A GCP project with sufficient TPU quotas for **v6e** and **tpu7x** in `us-central2-b`.
3.  **Permissions**: IAM permissions to manage cluster networks, subnetworks, firewalls, and node pools.

---

## Dynamic Namespace Isolation

To prevent ongoing test runs from conflicting or locking physical TPU resources, the E2E runner automatically allocates a unique, isolated test namespace (e.g. `test-ns-tfx7i`) for every run.

When the suite completes, it deletes the dynamic namespace to fully release the physical hardware and clean up cluster resources.

---

## Quick Start: Fully Automated Suite

Use this workflow if you want the framework to automatically provision a GKE cluster, configure resources, run tests, and tear down when finished.

### 1. Configure Environment
Define your project and regional preferences:
```bash
export PROJECT_ID=$(gcloud config get project)
export CLUSTER_NAME=ray-tpu-e2e-cluster
export REGION=us-central2
export ZONE=us-central2-b
```

### 2. Configuring Custom Ray Docker Images (Optional)
By default, the E2E test suite uses the official **`rayproject/ray:nightly-tpu`** image to test the webhook with the latest Ray builds.

You can easily override this to test a custom Ray image (or a specific version) by setting the `RAY_IMAGE` environment variable before running the suite:
```bash
# Example: Test a custom local or private registry image
export RAY_IMAGE="rayproject/ray:2.35.0-py310-tpu"
```

### 3. Provision and Test
Run the wrapper script with `--setup` to spin up the custom VPC, the GKE cluster, `cert-manager`, and the webhook:
```bash
./scripts/run-e2e.sh --setup
```
This provisions the cluster infrastructure, registers the webhook controllers, applies the test manifests, and executes all tests in logical groups.

### 3. Clean Up
To prevent ongoing GCP charges after testing:
```bash
./scripts/run-e2e.sh --teardown
```

---

## Manual Verification: Pre-existing Cluster

Use this workflow if you already have a running GKE cluster with the KubeRay TPU webhook active and want to run individual tests manually.

### 1. Deploy Manifests
Apply the RayCluster manifests to a specific namespace (e.g. `test-namespace`) to initiate mutation:
```bash
# For V6e TPUs:
kubectl apply -f e2e/manifests/v6e/v6e-8-single-host.yaml -n test-namespace
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-host.yaml -n test-namespace
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-slice.yaml -n test-namespace
kubectl apply -f e2e/manifests/v6e/heterogeneous-cluster.yaml -n test-namespace

# For V7x TPUs:
kubectl apply -f e2e/manifests/v7x/v7x-8-single-host.yaml -n test-namespace
kubectl apply -f e2e/manifests/v7x/v7x-16-multi-host.yaml -n test-namespace
kubectl apply -f e2e/manifests/v7x/v7x-16-multi-slice.yaml -n test-namespace
kubectl apply -f e2e/manifests/v7x/v7x-multi-container.yaml -n test-namespace
```

### 2. Run Assertions
Run the entire test suite via:
```bash
export TEST_NAMESPACE=test-namespace
make e2e
```

Or target specific test groups and scenarios directly:

```bash
cd e2e/webhook

# Validation, Single-Host, & Multi-Container E2E tests (Group 1)
go test -v -run "TestWebhookMutation_V6eSingleHost|TestRayClusterValidation|TestWebhookMutation_HeterogeneousCluster|TestWebhookMutation_V7xSingleHost|TestWebhookMutation_V7xMultiContainer"

# Multi-Host, DNS, & Pod Churn E2E tests (Group 2)
go test -v -run "TestWebhookMutation_V6eMultiHost|TestWebhookMutation_V6ePodChurnSingleSlice|TestWebhookMutation_V6eDNSResolution|TestWebhookMutation_V7xMultiHost"

# Multi-Slice (Megascale) & Multi-Slice Churn E2E tests (Group 3)
go test -v -run "TestWebhookMutation_V6eMultiSlice|TestWebhookMutation_V6ePodChurnMultiSlice|TestWebhookMutation_V7xMultiSlice"
```

### 3. Clean Up
Delete the test fixtures when finished:
```bash
kubectl delete ns test-namespace
```

---

## TPU7x Dynamic Slicing E2E Tests

Dynamic Slicing E2E tests require a TPU7x cluster with Dynamic Slicing (`PROVISION_ONLY` topology mode), Kueue, and the Kueue Slice Controller (`kueue-slice-controller`) installed. They are gated behind the `dynamic_slicing` Go build tag (`//go:build dynamic_slicing`) so they do not run during standard `-tags=e2e` runs.

Each test creates its own `RayCluster` or `RayJob` from `e2e/manifests/dynamic-slicing/`, ensures a `LocalQueue` exists in `TEST_NAMESPACE` (default `default`), and cleans up all workloads, pods, and active `Slice` reservations before the next test runs.

### Dynamic Slicing Cluster Requirements

Any Kubernetes cluster used to run the Dynamic Slicing E2E suite needs:
- **Kubernetes `1.32+`** with the `Slice` CRD (`slices.accelerator.gke.io`) and a slice provisioner active.
- **CPU nodes**: At least 1 CPU node (for example `n2-standard-8` or `e2-standard-8`) to host cluster controllers and Ray head pods.
- **TPU7x nodes in `PROVISION_ONLY` mode**:
  - 32 `tpu7x-standard-4t` nodes (128 chips total) across two `4x4x4` sub-block node pools (16 nodes / 64 chips each) on the same physical block to run all tests, including `TestDynamicSlicing_SuperSlice_4x4x8`.
  - A single 16-node (`4x4x4` sub-block) pool is sufficient to run all sub-slice, multi-slice, multi-group, multi-container, and `RayJob` tests if you skip `TestDynamicSlicing_SuperSlice_4x4x8`.
- **Controllers**:
  - `cert-manager` (for webhook TLS certificates)
  - KubeRay Operator (`v1.7.1+` recommended)
  - KubeRay TPU Webhook (deployed from this repository)
  - Kueue (`v0.20.2+`) with Topology-Aware Scheduling (TAS)
  - Kueue Slice Controller (`kueue-slice-controller`, controller name `accelerator.gke.io/slice`)

### Step-by-Step Cluster Setup

#### 1. Create the Cluster and Network

On GKE, create a custom-subnet VPC with an 8896 MTU, an internal firewall rule for multi-host Ray and TPU traffic, and a cluster with Dataplane V2, multi-networking, and the Slice Controller enabled:

```bash
export PROJECT_ID=<your-gcp-project>
export CLUSTER_NAME=tpu7x-dynamic-slicing-e2e
export REGION=us-central1
export ZONE=us-central1-c
export NETWORK_NAME=${CLUSTER_NAME}-net
export SUBNET_NAME=${CLUSTER_NAME}-subnet

gcloud compute networks create "${NETWORK_NAME}" \
  --project="${PROJECT_ID}" \
  --subnet-mode=custom \
  --mtu=8896

gcloud compute networks subnets create "${SUBNET_NAME}" \
  --project="${PROJECT_ID}" \
  --network="${NETWORK_NAME}" \
  --region="${REGION}" \
  --range=192.168.0.0/20

gcloud compute firewall-rules create "${NETWORK_NAME}-allow-internal" \
  --project="${PROJECT_ID}" \
  --network="${NETWORK_NAME}" \
  --allow=all \
  --source-ranges=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16

gcloud container clusters create "${CLUSTER_NAME}" \
  --project="${PROJECT_ID}" \
  --location="${ZONE}" \
  --release-channel=rapid \
  --machine-type=n2-standard-8 \
  --num-nodes=2 \
  --network="${NETWORK_NAME}" \
  --subnetwork="${SUBNET_NAME}" \
  --enable-dataplane-v2 \
  --enable-multi-networking \
  --enable-slice-controller
```

Fetch cluster credentials:

```bash
gcloud container clusters get-credentials "${CLUSTER_NAME}" \
  --zone="${ZONE}" \
  --project="${PROJECT_ID}"
```

#### 2. Provision TPU7x Node Pools in `PROVISION_ONLY` Mode

Create a Compute Engine `HIGH_THROUGHPUT` workload policy with `--accelerator-topology-mode=PROVISION_ONLY`. Unlike static TPU node pools, `PROVISION_ONLY` node pools do not form a fixed TPU slice at node creation time; instead, slices are formed dynamically on demand via `Slice` custom resources.

```bash
export WORKLOAD_POLICY=${CLUSTER_NAME}-tpu7x-policy

gcloud compute resource-policies create workload-policy "${WORKLOAD_POLICY}" \
  --project="${PROJECT_ID}" \
  --region="${REGION}" \
  --type=HIGH_THROUGHPUT \
  --accelerator-topology=4x4x4 \
  --accelerator-topology-mode=PROVISION_ONLY
```

Create two 16-node (`4x4x4`) TPU7x node pools attached to that placement policy (and targeting your TPU7x reservation if using reserved capacity):

```bash
for POOL in tpu-pool-sub1 tpu-pool-sub2; do
  gcloud container node-pools create "${POOL}" \
    --project="${PROJECT_ID}" \
    --cluster="${CLUSTER_NAME}" \
    --location="${ZONE}" \
    --machine-type=tpu7x-standard-4t \
    --num-nodes=16 \
    --placement-policy="${WORKLOAD_POLICY}" \
    --enable-gvnic \
    --scopes=https://www.googleapis.com/auth/cloud-platform
done
```

> **Note:** If you are using a specific TPU reservation, pass `--reservation-affinity=specific --reservation=<reservation-name>` when creating the node pools so both pools land in the same physical block for `4x4x8` SuperSlice tests.

#### 3. Install KubeRay Operator, `cert-manager`, and the KubeRay TPU Webhook

Install the KubeRay Operator with Helm (omitting `--version` installs the latest release):

```bash
helm repo add kuberay https://ray-project.github.io/kuberay-helm/
helm repo update
helm install kuberay-operator kuberay/kuberay-operator \
  --namespace ray-system \
  --create-namespace
```

Install `cert-manager` (if not already present) and deploy the KubeRay TPU Webhook from this repository:

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update
helm install --create-namespace --namespace cert-manager --set installCRDs=true --set global.leaderElection.namespace=cert-manager cert-manager jetstack/cert-manager
kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=300s

make deploy
make deploy-cert
kubectl wait --for=condition=Available deployment/kuberay-tpu-webhook -n ray-system --timeout=300s
```

#### 4. Install Kueue and the Kueue Slice Controller

1. Install Kueue (`v0.20.2+`):

   ```bash
   kubectl apply --server-side -f https://github.com/kubernetes-sigs/kueue/releases/download/v0.20.2/manifests.yaml
   kubectl wait --for=condition=Available deployment/kueue-controller-manager -n kueue-system --timeout=300s
   ```

2. Install the Kueue Slice Controller (`kueue-slice-controller`) into `slice-controller-system` by following the [GKE Dynamic Slicing guide](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/use-gke-dynamic-slicing#install_kueue_slice_controller). This controller reconciles the `accelerator.gke.io/slice` `AdmissionCheck` and mutates Kueue-managed `RayCluster` and `RayJob` workloads with TAS annotations. Wait for its deployment to become available:

   ```bash
   kubectl wait --for=condition=Available deployment/slice-controller-controller-manager \
     -n slice-controller-system --timeout=300s
   ```

#### 5. Configure Kueue Topology, Flavors, AdmissionCheck, and Queues

Apply the following Kueue and Topology-Aware Scheduling (TAS) resources. The E2E tests default to `ClusterQueue` `cq` and `LocalQueue` `lq`:

```yaml
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: superslice-topology
spec:
  levels:
  - nodeLabel: cloud.google.com/gce-topology-block
  - nodeLabel: cloud.google.com/gke-tpu-partition-4x4x4-id
  - nodeLabel: cloud.google.com/gke-tpu-partition-2x4x4-id
  - nodeLabel: cloud.google.com/gke-tpu-partition-2x2x4-id
  - nodeLabel: cloud.google.com/gke-tpu-partition-2x2x2-id
  - nodeLabel: kubernetes.io/hostname
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: tpu7x
spec:
  nodeLabels:
    cloud.google.com/gke-tpu-accelerator: tpu7x
  topologyName: superslice-topology
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: default-flavor
spec: {}
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: AdmissionCheck
metadata:
  name: gke-slice-provisioning
spec:
  controllerName: accelerator.gke.io/slice
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: cq
spec:
  namespaceSelector: {}
  queueingStrategy: BestEffortFIFO
  admissionChecksStrategy:
    admissionChecks:
    - name: gke-slice-provisioning
  resourceGroups:
  - coveredResources: ["google.com/tpu", "google.com/netdev"]
    flavors:
    - name: tpu7x
      resources:
      - name: google.com/tpu
        nominalQuota: "128"
      - name: google.com/netdev
        nominalQuota: "64"
  - coveredResources: ["cpu", "memory"]
    flavors:
    - name: default-flavor
      resources:
      - name: cpu
        nominalQuota: "100"
      - name: memory
        nominalQuota: "100Gi"
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: lq
  namespace: default
spec:
  clusterQueue: cq
```

#### 6. Verify Cluster Readiness

Confirm that the `AdmissionCheck` and `ClusterQueue` are `Active=True` and that TPU7x nodes carry the expected partition labels:

```bash
kubectl get admissioncheck gke-slice-provisioning
kubectl get clusterqueue cq
kubectl get localqueue lq -n default
kubectl get nodes -l cloud.google.com/gke-tpu-accelerator=tpu7x \
  -L cloud.google.com/gce-topology-block,cloud.google.com/gke-tpu-partition-4x4x4-id,cloud.google.com/gke-tpu-partition-2x2x2-id
```

### Running the Dynamic Slicing Tests

Run the full Dynamic Slicing suite against your current `kubectl` context:

```bash
make e2e-dynamic-slicing
# or directly:
go test -timeout 45m -tags=dynamic_slicing -count=1 -v ./e2e/webhook/...
```

To run the suite on a 16-node (`4x4x4`) cluster by skipping the 32-node SuperSlice test:

```bash
go test -timeout 45m -tags=dynamic_slicing -count=1 -v ./e2e/webhook/... -skip "^TestDynamicSlicing_SuperSlice_4x4x8$"
```

To run a single test scenario:

```bash
go test -timeout 25m -tags=dynamic_slicing -count=1 -v ./e2e/webhook/ -run "^TestDynamicSlicing_SubSlice_2x2x2$"
```

Optional environment variables:
- `TEST_NAMESPACE`: Namespace to run tests in (default `default`).
- `DYNAMIC_SLICING_LOCAL_QUEUE`: Kueue `LocalQueue` name (default `lq`).
- `DYNAMIC_SLICING_CLUSTER_QUEUE`: Kueue `ClusterQueue` name (default `cq`).
- `RAY_TPU7X_IMAGE`: Override the Ray TPU7x container image (default `rayproject/ray:nightly-py312-tpu`).
