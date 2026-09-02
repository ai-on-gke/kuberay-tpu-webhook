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
