#!/bin/bash
# scripts/setup-cluster.sh

set -e

# Required environment variables
PROJECT_ID=${PROJECT_ID:-$(gcloud config get project)}
CLUSTER_NAME=${CLUSTER_NAME:-ray-llm-cluster}
REGION=${REGION:-us-central1}
NETWORK_NAME=${NETWORK_NAME:-${CLUSTER_NAME}-net}

echo "Using Project: $PROJECT_ID"
echo "Using Cluster Name: $CLUSTER_NAME"
echo "Using Region: $REGION"
echo "Using Network: $NETWORK_NAME"

# Proceed with setting up VPC network, GKE cluster, and node pools


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
SUBNET_NAME="${NETWORK_NAME}-subnet-${REGION}"
if ! gcloud compute networks subnets describe "$SUBNET_NAME" --region "$REGION" >/dev/null 2>&1; then
    echo "Creating subnet $SUBNET_NAME..."
    gcloud compute --project="${PROJECT_ID}" \
        networks subnets create "${SUBNET_NAME}" \
        --network="${NETWORK_NAME}" \
        --region="${REGION}" \
        --range="${SUBNET_RANGE:-192.168.102.0/24}"
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

# Create GKE cluster if it doesn't exist
if ! gcloud container clusters describe "$CLUSTER_NAME" --region "$REGION" >/dev/null 2>&1; then
    echo "Creating GKE cluster $CLUSTER_NAME..."
    gcloud container clusters create "$CLUSTER_NAME" \
        --addons=RayOperator \
        --machine-type=n2-standard-8 \
        --enable-dataplane-v2 \
        --workload-pool="$PROJECT_ID.svc.id.goog" \
        --network="${NETWORK_NAME}" \
        --subnetwork="${SUBNET_NAME}" \
        --location="$REGION"
else
    echo "GKE cluster $CLUSTER_NAME already exists. Skipping cluster creation."
fi

# Provision multi-host TPU slice node pools for v6e
if ! gcloud container node-pools describe v6e-4x4 --cluster="$CLUSTER_NAME" --region="$REGION" >/dev/null 2>&1; then
    echo "Creating node pool v6e-4x4 (16 chips)..."
    gcloud container node-pools create v6e-4x4 \
        --location="$REGION" \
        --node-locations="us-central1-b" \
        --cluster="$CLUSTER_NAME" \
        --machine-type=ct6e-standard-4t \
        --threads-per-core=2 \
        --tpu-topology=4x4 \
        --num-nodes=4 \
        --enable-gvnic \
        --spot \
        --scopes=https://www.googleapis.com/auth/cloud-platform
else
    echo "Node pool v6e-4x4 already exists."
fi

if ! gcloud container node-pools describe v6e-2x4 --cluster="$CLUSTER_NAME" --region="$REGION" >/dev/null 2>&1; then
    echo "Creating node pool v6e-2x4 (8 chips)..."
    gcloud container node-pools create v6e-2x4 \
        --location="$REGION" \
        --node-locations="us-central1-b" \
        --cluster="$CLUSTER_NAME" \
        --machine-type=ct6e-standard-4t \
        --threads-per-core=2 \
        --tpu-topology=2x4 \
        --num-nodes=2 \
        --enable-gvnic \
        --spot \
        --scopes=https://www.googleapis.com/auth/cloud-platform
else
    echo "Node pool v6e-2x4 already exists."
fi

# Provision single-host and multi-host TPU node pools for tpu7x
if ! gcloud container node-pools describe tpu7x-2x2x1 --cluster="$CLUSTER_NAME" --region="$REGION" >/dev/null 2>&1; then
    echo "Creating node pool tpu7x-2x2x1 (single host)..."
    gcloud container node-pools create tpu7x-2x2x1 \
        --location="$REGION" \
        --node-locations="us-central1-c" \
        --cluster="$CLUSTER_NAME" \
        --machine-type=tpu7x-standard-4t \
        --num-nodes=1 \
        --enable-gvnic \
        --spot \
        --scopes=https://www.googleapis.com/auth/cloud-platform
else
    echo "Node pool tpu7x-2x2x1 already exists."
fi

if ! gcloud container node-pools describe tpu7x-2x2x2 --cluster="$CLUSTER_NAME" --region="$REGION" >/dev/null 2>&1; then
    WORKLOAD_POLICY_NAME="tpu7x-8-2x2x2-placement-policy"
    # Create the workload policy if it doesn't exist
    if ! gcloud compute resource-policies describe "$WORKLOAD_POLICY_NAME" --region="$REGION" >/dev/null 2>&1; then
        echo "Creating workload policy $WORKLOAD_POLICY_NAME..."
        gcloud compute resource-policies create workload-policy "$WORKLOAD_POLICY_NAME" \
            --type=HIGH_THROUGHPUT \
            --accelerator-topology=2x2x2 \
            --project="$PROJECT_ID" \
            --region="$REGION"
    else
        echo "Workload policy $WORKLOAD_POLICY_NAME already exists."
    fi

    echo "Creating node pool tpu7x-2x2x2 (multi host)..."
    gcloud container node-pools create tpu7x-2x2x2 \
        --location="$REGION" \
        --node-locations="us-central1-c" \
        --cluster="$CLUSTER_NAME" \
        --machine-type=tpu7x-standard-4t \
        --placement-policy="$WORKLOAD_POLICY_NAME" \
        --num-nodes=2 \
        --enable-gvnic \
        --spot \
        --scopes=https://www.googleapis.com/auth/cloud-platform
else
    echo "Node pool tpu7x-2x2x2 already exists."
fi

echo "Cluster setup complete."
