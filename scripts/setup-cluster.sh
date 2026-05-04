#!/bin/bash
# scripts/setup-cluster.sh

set -e

# Required environment variables
PROJECT_ID=${PROJECT_ID:-$(gcloud config get project)}
CLUSTER_NAME=${CLUSTER_NAME:-ray-llm-cluster}
REGION=${REGION:-us-central1}
ZONE=${ZONE:-us-central1-a}
NETWORK_NAME=${NETWORK_NAME:-${CLUSTER_NAME}-net}

echo "Using Project: $PROJECT_ID"
echo "Using Cluster Name: $CLUSTER_NAME"
echo "Using Region: $REGION"
echo "Using Zone: $ZONE"
echo "Using Network: $NETWORK_NAME"

# Check if cluster already exists
if gcloud container clusters describe "$CLUSTER_NAME" --zone "$ZONE" >/dev/null 2>&1; then
    echo "Cluster $CLUSTER_NAME already exists in $ZONE. Skipping creation."
    exit 0
fi

echo "Cluster not found. Proceeding with setup."

# Create VPC network if it doesn't exist
if ! gcloud compute networks describe "$NETWORK_NAME" >/dev/null 2>&1; then
    echo "Creating VPC network $NETWORK_NAME with MTU 8896..."
    gcloud compute --project="${PROJECT_ID}" \
        networks create "${NETWORK_NAME}" \
        --subnet-mode=custom \
        --mtu=8896
else
    echo "VPC network $NETWORK_NAME already exists."
fi

# Create subnet if it doesn't exist
SUBNET_NAME="${NETWORK_NAME}-subnet"
if ! gcloud compute networks subnets describe "$SUBNET_NAME" --region "$REGION" >/dev/null 2>&1; then
    echo "Creating subnet $SUBNET_NAME..."
    gcloud compute --project="${PROJECT_ID}" \
        networks subnets create "${SUBNET_NAME}" \
        --network="${NETWORK_NAME}" \
        --region="${REGION}" \
        --range=192.168.100.0/24
else
    echo "Subnet $SUBNET_NAME already exists."
fi

# Create firewall rules if they don't exist
FIREWALL_RULE="${NETWORK_NAME}-allow-internal"
if ! gcloud compute firewall-rules describe "$FIREWALL_RULE" >/dev/null 2>&1; then
    echo "Creating firewall rule $FIREWALL_RULE..."
    gcloud compute --project="${PROJECT_ID}" firewall-rules create "${FIREWALL_RULE}" \
        --network="${NETWORK_NAME}" \
        --allow=all \
        --source-ranges=172.16.0.0/12,192.168.0.0/16,10.0.0.0/8 \
        --description="Allow all internal traffic within the network."
else
    echo "Firewall rule $FIREWALL_RULE already exists."
fi

# Create GKE cluster
echo "Creating GKE cluster $CLUSTER_NAME..."
gcloud container clusters create "$CLUSTER_NAME" \
    --addons=RayOperator \
    --machine-type=e2-standard-8 \
    --enable-dataplane-v2 \
    --workload-pool="$PROJECT_ID.svc.id.goog" \
    --network="${NETWORK_NAME}" \
    --subnetwork="${SUBNET_NAME}" \
    --location="$ZONE"

# Provision multi-host TPU slice node pool (defaulting to v6e as in the guide)
echo "Creating node pool v6e-16..."
gcloud container node-pools create v6e-16 \
    --location="$ZONE" \
    --cluster="$CLUSTER_NAME" \
    --machine-type=ct6e-standard-4t \
    --threads-per-core=1 \
    --tpu-topology=4x4 \
    --num-nodes=4 \
    --enable-gvnic \
    --scopes=https://www.googleapis.com/auth/cloud-platform

echo "Cluster setup complete."
