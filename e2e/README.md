# E2E Mutation & Validation Tests for KubeRay TPU Webhook

This directory contains end-to-end (E2E) qualification and mutation tests to validate that the KubeRay TPU webhook correctly injects TPU-specific environment variables, labels, and hostnames on GKE TPU workloads.

---

## Prerequisites

1.  **Tools**: Install `gcloud`, `kubectl`, and Go `1.25+`.
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

# Run Single-Slice Pod Churn qualification tests (concurrently deletes 2 pods)
go test -v -run TestWebhookMutation_V6ePodChurnSingleSlice

# Run Multi-Slice Pod Churn qualification tests (concurrently deletes 4 pods across slices)
go test -v -run TestWebhookMutation_V6ePodChurnMultiSlice

# Run all Validating Webhook tests for RayClusters requesting TPUs
go test -v -run TestRayClusterValidation
```

### 3. Clean Up
After tests complete, clean up the deployed mutation test fixtures:
```bash
kubectl delete -f e2e/manifests/v6e/v6e-8-single-host.yaml --ignore-not-found=true || true
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-host.yaml --ignore-not-found=true || true
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-slice.yaml --ignore-not-found=true || true
```



