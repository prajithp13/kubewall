#!/bin/bash

# Script to create a readonly cluster role and service account for Kubewall remote access
# This script generates a Kubernetes secret that can be applied to the cluster where Kubewall is running

set -e

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Default values
NAMESPACE="default"
SERVICE_ACCOUNT_NAME="kubewall-readonly"
CLUSTER_ROLE_NAME="kubewall-readonly"
SECRET_NAME=""
CLUSTER_NAME=""

# Function to print colored messages
print_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

print_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

# Function to display usage
usage() {
    cat << EOF
Usage: $0 -c CLUSTER_NAME [-n NAMESPACE] [-s SERVICE_ACCOUNT_NAME] [-r CLUSTER_ROLE_NAME]

This script creates a readonly cluster role and service account for Kubewall remote access.
It generates a Kubernetes secret that can be applied to the cluster where Kubewall is running.

Required arguments:
    -c CLUSTER_NAME          Name of the cluster (used for secret name)

Optional arguments:
    -n NAMESPACE             Namespace for the service account (default: default)
    -s SERVICE_ACCOUNT_NAME  Name of the service account (default: kubewall-readonly)
    -r CLUSTER_ROLE_NAME     Name of the cluster role (default: kubewall-readonly)
    -h                       Display this help message

Example:
    $0 -c my-staging-cluster
    $0 -c production -n kube-system -s kubewall-sa

EOF
    exit 1
}

# Parse command line arguments
while getopts "c:n:s:r:h" opt; do
    case $opt in
        c) CLUSTER_NAME="$OPTARG" ;;
        n) NAMESPACE="$OPTARG" ;;
        s) SERVICE_ACCOUNT_NAME="$OPTARG" ;;
        r) CLUSTER_ROLE_NAME="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

# Validate required arguments
if [ -z "$CLUSTER_NAME" ]; then
    print_error "Cluster name is required"
    usage
fi

# Generate secret name from cluster name
SECRET_NAME="cluster-ct-${CLUSTER_NAME}"

print_info "Starting setup for cluster: $CLUSTER_NAME"
print_info "Namespace: $NAMESPACE"
print_info "Service Account: $SERVICE_ACCOUNT_NAME"
print_info "Cluster Role: $CLUSTER_ROLE_NAME"
print_info "Secret Name: $SECRET_NAME"
echo ""

# Check if kubectl is available
if ! command -v kubectl &> /dev/null; then
    print_error "kubectl is not installed or not in PATH"
    exit 1
fi

# Check if we can connect to the cluster
if ! kubectl cluster-info &> /dev/null; then
    print_error "Cannot connect to Kubernetes cluster. Please check your kubeconfig"
    exit 1
fi

print_info "Step 1: Creating namespace if it doesn't exist..."
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - > /dev/null 2>&1

print_info "Step 2: Creating ClusterRole with readonly permissions..."
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $CLUSTER_ROLE_NAME
rules:
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["get", "list", "watch"]
- nonResourceURLs: ["*"]
  verbs: ["get", "list"]
EOF

print_info "Step 3: Creating ServiceAccount..."
kubectl create serviceaccount "$SERVICE_ACCOUNT_NAME" -n "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - > /dev/null

print_info "Step 4: Creating ClusterRoleBinding..."
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $CLUSTER_ROLE_NAME-binding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $CLUSTER_ROLE_NAME
subjects:
- kind: ServiceAccount
  name: $SERVICE_ACCOUNT_NAME
  namespace: $NAMESPACE
EOF

print_info "Step 5: Creating ServiceAccount token secret..."
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: ${SERVICE_ACCOUNT_NAME}-token
  namespace: $NAMESPACE
  annotations:
    kubernetes.io/service-account.name: $SERVICE_ACCOUNT_NAME
type: kubernetes.io/service-account-token
EOF

# Wait for token to be populated
print_info "Step 6: Waiting for token to be generated..."
for i in {1..30}; do
    TOKEN=$(kubectl get secret "${SERVICE_ACCOUNT_NAME}-token" -n "$NAMESPACE" -o jsonpath='{.data.token}' 2>/dev/null || echo "")
    if [ -n "$TOKEN" ]; then
        break
    fi
    sleep 1
done

if [ -z "$TOKEN" ]; then
    print_error "Failed to get service account token after 30 seconds"
    exit 1
fi

print_info "Step 7: Extracting cluster information..."

# Get the bearer token
BEARER_TOKEN=$(echo "$TOKEN" | base64 -d)

# Get the CA certificate
CA_DATA=$(kubectl get secret "${SERVICE_ACCOUNT_NAME}-token" -n "$NAMESPACE" -o jsonpath='{.data.ca\.crt}')

# Get the server URL
SERVER_URL=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')

if [ -z "$SERVER_URL" ]; then
    print_error "Failed to get server URL from kubeconfig"
    exit 1
fi

print_info "Step 8: Generating Kubewall secret manifest..."

# Create the config JSON
CONFIG_JSON=$(cat <<EOF
{
  "bearerToken": "$BEARER_TOKEN",
  "tlsClientConfig": {
    "insecure": false,
    "caData": "$CA_DATA"
  }
}
EOF
)

# Base64 encode the config JSON
CONFIG_BASE64=$(echo -n "$CONFIG_JSON" | base64 -w 0)

# Base64 encode the cluster name
NAME_BASE64=$(echo -n "$CLUSTER_NAME" | base64 -w 0)

# Base64 encode the server URL
SERVER_BASE64=$(echo -n "$SERVER_URL" | base64 -w 0)

# Generate the secret YAML
OUTPUT_FILE="${SECRET_NAME}.yaml"

cat > "$OUTPUT_FILE" <<EOF
apiVersion: v1
kind: Secret
metadata:
  labels:
    kubewall/cluster: "true"
  name: $SECRET_NAME
  namespace: default
type: Opaque
data:
  config: $CONFIG_BASE64
  name: $NAME_BASE64
  server: $SERVER_BASE64
EOF

print_info "✓ Secret manifest generated successfully!"
echo ""
print_info "Secret file created: $OUTPUT_FILE"
echo ""
print_warning "To apply this secret to the cluster where Kubewall is running, execute:"
echo -e "${GREEN}kubectl apply -f $OUTPUT_FILE${NC}"
echo ""
print_info "Summary:"
echo "  Cluster Name: $CLUSTER_NAME"
echo "  Server URL: $SERVER_URL"
echo "  Service Account: $SERVICE_ACCOUNT_NAME (namespace: $NAMESPACE)"
echo "  Secret Name: $SECRET_NAME"
echo ""
print_info "The secret contains:"
echo "  - config: JSON with bearerToken and TLS configuration"
echo "  - name: Cluster name"
echo "  - server: Kubernetes API server URL"
