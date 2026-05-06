#!/bin/bash
# scripts/run-e2e.sh

set -e

# Required environment variables
PROJECT_ID=${PROJECT_ID:-$(gcloud config get project)}
CLUSTER_NAME=${CLUSTER_NAME:-ray-llm-cluster}
ZONE=${ZONE:-us-central1-a}
NAMESPACE=${NAMESPACE:-default}

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
kubectl apply -f deployments/webhook-svc.yaml
kubectl apply -f deployments/mutating-webhook-cfg.yaml
kubectl apply -f deployments/validating-webhook-cfg.yaml
kubectl apply -f deployments/deployment.yaml

# Wait for webhook to be ready
echo "Waiting for webhook to be ready..."
kubectl wait --for=condition=Available deployment/kuberay-tpu-webhook -n ray-system --timeout=300s

# Run tests
echo "Running tests..."
go test -v ./e2e/webhook/...

# Teardown if requested
if [ "$TEARDOWN_CLUSTER" = true ]; then
    echo "Tearing down cluster $CLUSTER_NAME..."
    gcloud container clusters delete "$CLUSTER_NAME" --zone "$ZONE" --quiet
    echo "Tearing down network ${CLUSTER_NAME}-net..."
    gcloud compute networks delete "${CLUSTER_NAME}-net" --quiet
fi
