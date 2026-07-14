#!/bin/bash
# scripts/run-e2e.sh

set -e

# Required environment variables
PROJECT_ID=${PROJECT_ID:-$(gcloud config get project)}
CLUSTER_NAME=${CLUSTER_NAME:-ray-llm-cluster}
NAMESPACE=${NAMESPACE:-default}
RAY_IMAGE=${RAY_IMAGE:-rayproject/ray:nightly-tpu}
RAY_TPU7X_IMAGE=${RAY_TPU7X_IMAGE:-rayproject/ray:nightly-py312-tpu}
if [ "$NAMESPACE" = "default" ]; then
    NAMESPACE="test-ns-$(head /dev/urandom | tr -dc a-z0-9 | head -c 5)"
fi
export TEST_NAMESPACE="$NAMESPACE"
REGION=${REGION:-us-central1}
NETWORK_NAME=${NETWORK_NAME:-${CLUSTER_NAME}-net}
SUBNET_NAME=${SUBNET_NAME:-${NETWORK_NAME}-subnet-${REGION}}

# Parse flags
SETUP_CLUSTER=false
TEARDOWN_CLUSTER=false

while [[ "$#" -gt 0 ]]; do
    case $1 in
        --setup) SETUP_CLUSTER=true ;;
        --teardown) TEARDOWN_CLUSTER=true ;;
        *) echo "Unknown parameter passed: $1"; exit 1 ;;
    esac
    shift
done

# Setup cluster if requested
if [ "$SETUP_CLUSTER" = true ]; then
    echo "Setting up cluster..."
    ./scripts/setup-cluster.sh
fi

# Get credentials
echo "Getting credentials for cluster $CLUSTER_NAME..."
gcloud container clusters get-credentials "$CLUSTER_NAME" --region "$REGION"

# Install cert-manager - required for OSS webhook.
echo "Checking for cert-manager..."
if ! kubectl get deployment cert-manager -n cert-manager >/dev/null 2>&1; then
    echo "Installing cert-manager..."
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.12.0/cert-manager.yaml
    echo "Waiting for cert-manager to be ready..."
    kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=300s
else
    echo "cert-manager already installed."
fi

# Install webhook and certificate issuer
echo "Installing webhook..."
kubectl apply -f deployments/deployment.yaml
kubectl apply -f deployments/webhook-svc.yaml
kubectl apply -f deployments/mutating-webhook-cfg.yaml
kubectl apply -f deployments/validating-webhook-cfg.yaml
kubectl apply -f certs/

# Wait for webhook to be ready
echo "Waiting for webhook to be ready..."
kubectl wait --for=condition=Available deployment/kuberay-tpu-webhook -n ray-system --timeout=300s

# Wait for any terminating test namespaces from previous runs to fully clean up (prevents GKE TPU hardware resource locks)
echo "Checking for any terminating namespaces in the cluster..."
while kubectl get ns -o json | grep -q '"phase": "Terminating"'; do
    echo "Waiting for terminating namespaces to fully release GKE TPU hardware..."
    sleep 5
done
echo "Cluster is clean of terminating namespaces."

# Create isolated test namespace
echo "Creating isolated test namespace $NAMESPACE..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Initialize final exit code
TEST_EXIT_CODE=0

# 1. Run Single-Host, Validation, Heterogeneous, and V7x Single-Host/Multi-Container tests
echo "Deploying Single-Host, Heterogeneous, and V7x test manifests inside namespace $NAMESPACE..."
cat e2e/manifests/v6e/v6e-8-single-host.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
cat e2e/manifests/v6e/heterogeneous-cluster.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
cat e2e/manifests/tpu7x/tpu7x-8-single-host.yaml | sed "s|rayproject/ray:nightly-py312-tpu|$RAY_TPU7X_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
cat e2e/manifests/tpu7x/tpu7x-multi-container.yaml | sed "s|rayproject/ray:nightly-py312-tpu|$RAY_TPU7X_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
set +e
echo "Running Validation, Single-Host, & Multi-Container E2E tests (Group 1)..."
go test -timeout 60m -tags=e2e -count=1 -v ./e2e/webhook/... -run "TestWebhookMutation_V6eSingleHost|TestRayClusterValidation|TestWebhookMutation_HeterogeneousCluster|TestWebhookMutation_V7xSingleHost|TestWebhookMutation_V7xMultiContainer"
GROUP1_EXIT=$?
set -e
echo "Cleaning up Validation, Single-Host, & Multi-Container manifests..."
kubectl delete -f e2e/manifests/v6e/v6e-8-single-host.yaml -n "$NAMESPACE" --ignore-not-found=true || true
kubectl delete -f e2e/manifests/v6e/heterogeneous-cluster.yaml -n "$NAMESPACE" --ignore-not-found=true || true
kubectl delete -f e2e/manifests/tpu7x/tpu7x-8-single-host.yaml -n "$NAMESPACE" --ignore-not-found=true || true
kubectl delete -f e2e/manifests/tpu7x/tpu7x-multi-container.yaml -n "$NAMESPACE" --ignore-not-found=true || true
if [ $GROUP1_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$GROUP1_EXIT
fi


# 2. Run Multi-Host, Single-Slice Churn, DNS, and V7x Multi-Host tests
echo "Deploying Multi-Host and V7x Multi-Host manifests inside namespace $NAMESPACE..."
cat e2e/manifests/v6e/v6e-16-multi-host.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
cat e2e/manifests/tpu7x/tpu7x-16-multi-host.yaml | sed "s|rayproject/ray:nightly-py312-tpu|$RAY_TPU7X_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
set +e
echo "Running Multi-Host, DNS, & Pod Churn E2E tests (Group 2)..."
go test -timeout 60m -tags=e2e -count=1 -v ./e2e/webhook/... -run "TestWebhookMutation_V6eMultiHost|TestWebhookMutation_V6ePodChurnSingleSlice|TestWebhookMutation_V6eDNSResolution|TestWebhookMutation_V7xMultiHost"
GROUP2_EXIT=$?
set -e
echo "Cleaning up Multi-Host manifests..."
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-host.yaml -n "$NAMESPACE" --ignore-not-found=true || true
kubectl delete -f e2e/manifests/tpu7x/tpu7x-16-multi-host.yaml -n "$NAMESPACE" --ignore-not-found=true || true
if [ $GROUP2_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$GROUP2_EXIT
fi


# 3. Run Megascale Multi-Slice, Multi-Slice Churn, and V7x Multi-Slice tests
echo "Deploying Megascale Multi-Slice and V7x Multi-Slice manifests inside namespace $NAMESPACE..."
cat e2e/manifests/v6e/v6e-16-multi-slice.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
cat e2e/manifests/tpu7x/tpu7x-16-multi-slice.yaml | sed "s|rayproject/ray:nightly-py312-tpu|$RAY_TPU7X_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
set +e
echo "Running Multi-Slice (Megascale) & Multi-Slice Churn E2E tests (Group 3)..."
go test -timeout 60m -tags=e2e -count=1 -v ./e2e/webhook/... -run "TestWebhookMutation_V6eMultiSlice|TestWebhookMutation_V6ePodChurnMultiSlice|TestWebhookMutation_V7xMultiSlice"
GROUP3_EXIT=$?
set -e
echo "Cleaning up Multi-Slice manifests..."
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-slice.yaml -n "$NAMESPACE" --ignore-not-found=true || true
kubectl delete -f e2e/manifests/tpu7x/tpu7x-16-multi-slice.yaml -n "$NAMESPACE" --ignore-not-found=true || true
if [ $GROUP3_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$GROUP3_EXIT
fi


# 4. Run JAX/XLA and Ray Core TPU Utilities Integration tests
echo "Deploying JAX & Ray Core Utilities E2E integration manifests inside namespace $NAMESPACE..."
cat e2e/manifests/v6e/v6e-integration-tpu-utils.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
set +e
echo "Running JAX/XLA & Ray Core TPU Utilities E2E tests (Group 4)..."
go test -timeout 60m -tags=e2e -count=1 -v ./e2e/webhook/... -run "TestWebhookIntegration_RayTPUUtilsAndJAX"
GROUP4_EXIT=$?
set -e
echo "Cleaning up JAX & Ray Core Utilities manifests..."
kubectl delete -f e2e/manifests/v6e/v6e-integration-tpu-utils.yaml -n "$NAMESPACE" --ignore-not-found=true || true
	if [ $GROUP4_EXIT -ne 0 ]; then
		TEST_EXIT_CODE=$GROUP4_EXIT
	fi

	# 5. Run RayJob Ray Train + JAX integration tests
	echo "Deploying RayJob JAX integration manifests inside namespace $NAMESPACE..."
	kubectl create configmap rayjob-script --from-file=jax_trainer.py=e2e/scripts/jax_trainer.py -n "$NAMESPACE"
	cat e2e/manifests/v6e/v6e-jax-train-rayjob.yaml | sed "s|rayproject/ray:nightly-tpu|$RAY_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
	cat e2e/manifests/tpu7x/tpu7x-jax-train-rayjob.yaml | sed "s|rayproject/ray:nightly-py312-tpu|$RAY_TPU7X_IMAGE|g" | kubectl apply -n "$NAMESPACE" -f -
	set +e
	echo "Running RayJob JAX integration tests (Group 5)..."
	go test -timeout 60m -tags=e2e -count=1 -v ./e2e/webhook/... -run "TestRayJobIntegration"
	GROUP5_EXIT=$?
	set -e
	echo "Cleaning up RayJob integration manifests..."
	kubectl delete -f e2e/manifests/v6e/v6e-jax-train-rayjob.yaml -n "$NAMESPACE" --ignore-not-found=true || true
	kubectl delete -f e2e/manifests/tpu7x/tpu7x-jax-train-rayjob.yaml -n "$NAMESPACE" --ignore-not-found=true || true
	if [ $GROUP5_EXIT -ne 0 ]; then
		TEST_EXIT_CODE=$GROUP5_EXIT
	fi
# Clean up dynamic isolated test namespace
echo "Deleting isolated test namespace $NAMESPACE..."
kubectl delete namespace "$NAMESPACE" --ignore-not-found=true || true

# Propagate test exit failure if any
if [ $TEST_EXIT_CODE -ne 0 ]; then
    echo "Tests failed with exit code $TEST_EXIT_CODE"
    exit $TEST_EXIT_CODE
fi


# Teardown GKE resources if requested and script finished successfully
if [ "$TEARDOWN_CLUSTER" = true ]; then
    echo "Tearing down cluster $CLUSTER_NAME..."
    gcloud container clusters delete "$CLUSTER_NAME" --region "$REGION" --quiet || true
    echo "Tearing down firewall rule ${NETWORK_NAME}-allow-internal..."
    gcloud compute firewall-rules delete "${NETWORK_NAME}-allow-internal" --quiet || true
    echo "Tearing down subnet ${SUBNET_NAME}..."
    gcloud compute networks subnets delete "${SUBNET_NAME}" --region="${REGION}" --quiet || true
    echo "Tearing down network ${NETWORK_NAME}..."
    gcloud compute networks delete "${NETWORK_NAME}" --quiet || true
fi
