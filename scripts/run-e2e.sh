#!/bin/bash
# scripts/run-e2e.sh

set -e

# Required environment variables
PROJECT_ID=${PROJECT_ID:-$(gcloud config get project)}
CLUSTER_NAME=${CLUSTER_NAME:-ray-llm-cluster}
ZONE=${ZONE:-us-central2-b}
NAMESPACE=${NAMESPACE:-default}
REGION=${REGION:-us-central2}
NETWORK_NAME=${NETWORK_NAME:-${CLUSTER_NAME}-net}
SUBNET_NAME=${SUBNET_NAME:-${NETWORK_NAME}-subnet}

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
gcloud container clusters get-credentials "$CLUSTER_NAME" --zone "$ZONE"

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

# Initialize final exit code
TEST_EXIT_CODE=0

# 1. Run Single-Host and Validation test sequence
echo "Deploying Single-Host test manifest..."
kubectl apply -f e2e/manifests/v6e/v6e-8-single-host.yaml
set +e
echo "Running Single-Host and Validation tests..."
go test -v ./e2e/webhook/... -run "TestWebhookMutation_V6eSingleHost|TestRayClusterValidation"
SINGLE_HOST_EXIT=$?
set -e
echo "Cleaning up Single-Host test manifest..."
kubectl delete -f e2e/manifests/v6e/v6e-8-single-host.yaml --ignore-not-found=true || true
if [ $SINGLE_HOST_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$SINGLE_HOST_EXIT
fi

# 2. Run Multi-Host and Single-Slice Churn test sequence
echo "Deploying Multi-Host test manifest..."
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-host.yaml
set +e
echo "Running Multi-Host and Single-Slice Churn tests..."
go test -v ./e2e/webhook/... -run "TestWebhookMutation_V6eMultiHost|TestWebhookMutation_V6ePodChurnSingleSlice"
MULTI_HOST_EXIT=$?
set -e
echo "Cleaning up Multi-Host test manifest..."
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-host.yaml --ignore-not-found=true || true
if [ $MULTI_HOST_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$MULTI_HOST_EXIT
fi

# 3. Run Megascale Multi-Slice and Multi-Slice Churn test sequence
echo "Deploying Megascale Multi-Slice test manifest..."
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-slice.yaml
set +e
echo "Running Megascale Multi-Slice and Multi-Slice Churn tests..."
go test -v ./e2e/webhook/... -run "TestWebhookMutation_V6eMultiSlice|TestWebhookMutation_V6ePodChurnMultiSlice"
MULTI_SLICE_EXIT=$?
set -e
echo "Cleaning up Megascale Multi-Slice test manifest..."
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-slice.yaml --ignore-not-found=true || true
if [ $MULTI_SLICE_EXIT -ne 0 ]; then
    TEST_EXIT_CODE=$MULTI_SLICE_EXIT
fi

# Propagate test exit failure if any
if [ $TEST_EXIT_CODE -ne 0 ]; then
    echo "Tests failed with exit code $TEST_EXIT_CODE"
    exit $TEST_EXIT_CODE
fi


# Teardown if requested and script finished successfully
if [ "$TEARDOWN_CLUSTER" = true ]; then
    echo "Tearing down cluster $CLUSTER_NAME..."
    gcloud container clusters delete "$CLUSTER_NAME" --zone "$ZONE" --quiet || true
    echo "Tearing down firewall rule ${NETWORK_NAME}-allow-internal..."
    gcloud compute firewall-rules delete "${NETWORK_NAME}-allow-internal" --quiet || true
    echo "Tearing down subnet ${SUBNET_NAME}..."
    gcloud compute networks subnets delete "${SUBNET_NAME}" --region="${REGION}" --quiet || true
    echo "Tearing down network ${NETWORK_NAME}..."
    gcloud compute networks delete "${NETWORK_NAME}" --quiet || true
fi
