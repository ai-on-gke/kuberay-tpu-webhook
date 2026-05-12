# E2E Mutation & Validation Tests for KubeRay TPU Webhook

This directory contains end-to-end (E2E) qualification and mutation tests to validate that the KubeRay TPU webhook correctly injects TPU-specific environment variables, labels, and hostnames on GKE TPU workloads.

---

## Prerequisites

1.  **Tools**: Install `gcloud`, `kubectl`, and Go `1.22+`.
2.  **GCP Project & Quota**: A GCP project with sufficient TPU quota for **v6e** in `us-central2-b`.
3.  **Permissions**: IAM permissions to create clusters, networks, subnetworks, firewalls, and node pools.

---

## Quick Start: Fully Automated Suite

Use this workflow if you want the framework to automatically provision a GKE cluster, set up resources, run mutation tests, and tear down when finished.

### 1. Configure Environment
Define your project and regional preferences:
```bash
export PROJECT_ID=$(gcloud config get project)
export CLUSTER_NAME=ray-tpu-e2e-cluster
export REGION=us-central2
export ZONE=us-central2-b
```

### 2. Provision and Test
Run the wrapper script with `--setup` to spin up the custom VPC, the GKE cluster with `n2-standard-8` head nodes, a spot `v6e-16` TPU slice node pool, `cert-manager`, and the webhook:
```bash
./scripts/run-e2e.sh --setup
```
This will provision the infrastructure, register the webhook mutation controllers, apply manifests, and execute the Go test suite.

### 3. Clean Up
To prevent ongoing GCP charges after testing:
```bash
./scripts/run-e2e.sh --teardown
```

---

## Manual Verification: Pre-existing Cluster

Use this workflow if you already have a running GKE cluster with the KubeRay TPU webhook active and simply want to trigger mutation verification.

### 1. Deploy Manifests
Apply the RayCluster manifests to the cluster to initiate the mutating admission processes:
```bash
# Mutating Webhook positive scenarios
kubectl apply -f e2e/manifests/v6e/v6e-8-single-host.yaml
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-host.yaml
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-slice.yaml
```

### 2. Run Assertions
You can trigger the entire test suite via the root Makefile:
```bash
make e2e
```
Or navigate to the test directory and target individual verification scenarios:
```bash
cd e2e/webhook

# Run Single-Host mutation tests
go test -v -run TestWebhookMutation_V6eSingleHost

# Run Multi-Host mutation tests
go test -v -run TestWebhookMutation_V6eMultiHost

# Run Megascale Multi-Slice mutation tests
go test -v -run TestWebhookMutation_V6eMultiSlice

# Run Pod Churn qualification tests
go test -v -run TestWebhookMutation_V6ePodChurn

# Run Validating Webhook rejection tests
go test -v -run TestRayClusterValidation_InvalidTopology
```

---

## What the Tests Verify

The Go test suite encompasses both **Mutating Webhook** and **Validating Webhook** verification cases:

### 1. Mutation Tests (`tpu_pod_mutation_test.go`)
Queries the Kubernetes API server for generated worker pods and asserts that the mutating webhook correctly injected:
*   **Environment Variables**: `TPU_WORKER_ID` (unique index 0 to N-1), `TPU_NAME` (shared TPU slice name), `TPU_DEVICE_PLUGIN_HOST_IP`, and `TPU_DEVICE_PLUGIN_ADDR`.
*   **Hostnames & Pod Identity**: Pod subdomains and custom hostnames for multi-host configuration.
*   **Labels & Annotations**: Propagation of `replicaIndex` and native `ray.io/` indexing labels.
*   **Topology & Affinities**: Pod co-location constraints and affinities for scheduling worker groups to node pools.
*   **Multi-Slice Topologies**: Verifies multi-slice Megascale variable injection (`MEGASCALE_SLICE_ID`, `MEGASCALE_COORDINATOR_ADDRESS`, `MEGASCALE_PORT`) when `MEGASCALE_NUM_SLICES` is defined in the worker group specification.
*   **Pod Churn**: Deletes an active worker pod in a running multi-host cluster, waits for the KubeRay operator to automatically re-create it, and asserts that the validating mutating webhook dynamically detects the churn and assigns the **exact same** missing `TPU_WORKER_ID` to the new pod to restore the sequential order without interrupting GKE slice coordination.

### 2. Validation Tests (`tpu_raycluster_validation_test.go`)
Attempts to submit invalid RayCluster manifest configurations (e.g., [v6e-invalid-topology.yaml](file:///usr/local/google/home/ryanaoleary/Desktop/forks/kuberay-tpu-webhook/e2e/manifests/v6e/v6e-invalid-topology.yaml) where `numOfHosts` is mismatched with the TPU node selector and topology spec). It asserts that:
*   The API server **rejects** the creation of the RayCluster.
*   The rejected message contains: `"Number of workers in worker group not equal to specified topology"`.

