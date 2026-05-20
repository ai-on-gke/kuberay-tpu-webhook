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
if ! kubectl get pods -n cert-manager >/dev/null 2>&1; then
    echo "Installing cert-manager..."
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.12.0/cert-manager.yaml
    echo "Waiting for cert-manager to be ready..."
    kubectl wait --for=condition=Available deployment --all -n cert-manager --timeout=300s
else
    echo "cert-manager already installed."
fi

# Install webhook
echo "Installing webhook..."
kubectl apply -f deployments/deployment.yaml
kubectl apply -f deployments/webhook-svc.yaml
kubectl apply -f deployments/mutating-webhook-cfg.yaml
kubectl apply -f deployments/validating-webhook-cfg.yaml

# Wait for webhook to be ready
echo "Waiting for webhook to be ready..."
kubectl wait --for=condition=Available deployment/kuberay-tpu-webhook -n ray-system --timeout=300s

# Deploy mutation manifests
echo "Deploying RayCluster mutation test manifests..."
kubectl apply -f e2e/manifests/v6e/v6e-8-single-host.yaml
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-host.yaml
kubectl apply -f e2e/manifests/v6e/v6e-16-multi-slice.yaml

# Run tests with temporary set +e to guarantee cleanup
set +e
echo "Running tests..."
go test -v ./e2e/webhook/...
TEST_EXIT_CODE=$?
set -e

# Clean up mutation test manifests
echo "Cleaning up RayCluster mutation test manifests..."
kubectl delete -f e2e/manifests/v6e/v6e-8-single-host.yaml --ignore-not-found=true || true
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-host.yaml --ignore-not-found=true || true
kubectl delete -f e2e/manifests/v6e/v6e-16-multi-slice.yaml --ignore-not-found=true || true

# Propagate test exit failure if any
if [ $TEST_EXIT_CODE -ne 0 ]; then
    echo "Tests failed with exit code $TEST_EXIT_CODE"
    exit $TEST_EXIT_CODE
fi

# Teardown if requested
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
