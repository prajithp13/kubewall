# Cluster Access Setup Script

This directory contains scripts for setting up remote cluster access for Kubewall.

## setup-remote-cluster.sh

This script creates a readonly cluster role and service account for Kubewall remote access. It generates a Kubernetes secret that can be applied to the cluster where Kubewall is running.

### Prerequisites

- `kubectl` installed and configured with access to the target cluster
- Appropriate permissions to create ClusterRoles, ServiceAccounts, and Secrets

### Usage

```bash
./setup-remote-cluster.sh -c CLUSTER_NAME [-n NAMESPACE] [-s SERVICE_ACCOUNT_NAME] [-r CLUSTER_ROLE_NAME]
```

#### Required Arguments

- `-c CLUSTER_NAME`: Name of the cluster (used for secret name)

#### Optional Arguments

- `-n NAMESPACE`: Namespace for the service account (default: `default`)
- `-s SERVICE_ACCOUNT_NAME`: Name of the service account (default: `kubewall-readonly`)
- `-r CLUSTER_ROLE_NAME`: Name of the cluster role (default: `kubewall-readonly`)
- `-h`: Display help message

### Examples

Basic usage with just cluster name:
```bash
./setup-remote-cluster.sh -c my-staging-cluster
```

Custom namespace and service account:
```bash
./setup-remote-cluster.sh -c production -n kube-system -s kubewall-sa
```

### What the Script Does

1. **Creates a ClusterRole** with readonly permissions:
   - `get`, `list`, `watch` verbs for all API resources
   - `get`, `list` for non-resource URLs

2. **Creates a ServiceAccount** in the specified namespace

3. **Creates a ClusterRoleBinding** to bind the ClusterRole to the ServiceAccount

4. **Generates a ServiceAccount token** secret

5. **Extracts cluster information**:
   - Bearer token
   - CA certificate data
   - Kubernetes API server URL

6. **Generates a Kubernetes Secret manifest** with the format:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     labels:
       kubewall/cluster: "true"
     name: cluster-ct-<CLUSTER_NAME>
     namespace: default
   type: Opaque
   data:
     config: <base64-encoded-json>
     name: <base64-encoded-cluster-name>
     server: <base64-encoded-server-url>
   ```

   The `config` field contains base64-encoded JSON:
   ```json
   {
     "bearerToken": "<token>",
     "tlsClientConfig": {
       "insecure": false,
       "caData": "<ca-certificate>"
     }
   }
   ```

### Output

The script generates a YAML file named `cluster-ct-<CLUSTER_NAME>.yaml` in the current directory. This file can be applied to the cluster where Kubewall is running:

```bash
kubectl apply -f cluster-ct-<CLUSTER_NAME>.yaml
```

### Security Considerations

- The generated service account has **readonly access** to all resources in the cluster
- The bearer token provides cluster-wide read access
- Store the generated secret file securely
- Consider using namespace-specific roles if you need more restricted access
- The secret should be applied only to trusted Kubewall instances

### Troubleshooting

**Token not generated:**
- The script waits up to 30 seconds for the token to be generated
- If it fails, check if the service account token controller is running in your cluster

**Cannot connect to cluster:**
- Ensure your kubeconfig is properly configured
- Verify you have the necessary permissions to create cluster-level resources

**Permission denied:**
- Make sure the script is executable: `chmod +x setup-remote-cluster.sh`
- Verify you have cluster-admin or equivalent permissions
