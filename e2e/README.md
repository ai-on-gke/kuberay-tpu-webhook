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
This will provision the infrastructure, register the webhook mutation controllers, apply fixtures, and execute the Go test suite.

### 3. Clean Up
To prevent ongoing GCP charges after testing:
```bash
./scripts/run-e2e.sh --teardown
```

---

## Manual Verification: Pre-existing Cluster

Use this workflow if you already have a running GKE cluster with the KubeRay TPU webhook active and simply want to trigger mutation verification.

### 1. Deploy Fixtures
Apply the RayCluster fixtures to trigger pod mutations:
```bash
kubectl apply -f e2e/fixtures/v6e/v6e-8-single-host.yaml
kubectl apply -f e2e/fixtures/v6e/v6e-16-multi-host.yaml
```

### 2. Run Assertions
Use the Makefile target to trigger the test suite:
```bash
make e2e
```
Or run via Go directly:
```bash
cd e2e/webhook
go test -v ./...
```

---

## What the Tests Verify

The Go test suite queries the Kubernetes API server for generated pods and asserts that the mutating webhook correctly injected the following environment variables and configurations:

| Component / Feature | Verification Scope |
| :--- | :--- |
| **Environment Variables** | `TPU_WORKER_ID` (0 to N-1 unique indices), `TPU_NAME` (shared slice name), `TPU_DEVICE_PLUGIN_HOST_IP`, `TPU_DEVICE_PLUGIN_ADDR`. |
| **Hostnames & Pod Identity** | Pod subdomain and custom hostname mapping for multi-host configurations. |
| **Labels & Annotations** | Propagation of `replicaIndex` and `ray.io/` controller tags for gang scheduling. |
| **Topology & Affinities** | Co-location constraints, pod affinities, and slice grouping. |
