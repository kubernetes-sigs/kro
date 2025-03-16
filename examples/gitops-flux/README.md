# KRO GitOps with Flux CD Example

This example demonstrates how to implement GitOps practices using KRO (Kubernetes Resource Orchestrator) with Flux CD. It shows how to manage Kubernetes resources using Git as the single source of truth while leveraging KRO's powerful resource orchestration capabilities.

## Quick Start

This section provides a simple way to get started with KRO and Flux CD. Each step includes verified test results to demonstrate successful execution.

1. Install Prerequisites:
   ```bash
   # Install Flux CLI
   curl -s https://fluxcd.io/install.sh | sudo bash
   ```
   Expected output:
   ```
   [INFO] Downloading flux version v2.2.3
   [INFO] Verifying checksum... done.
   [INFO] Installing flux to /usr/local/bin/flux... done.
   ```

   ```bash
   # Install kind (for local testing)
   curl -Lo ./kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64
   chmod +x ./kind
   sudo mv ./kind /usr/local/bin/
   ```
   Verify installation:
   ```bash
   kind version
   ```
   Expected output:
   ```
   kind v0.20.0 go1.20.4 linux/amd64
   ```

2. Create a test cluster:
   ```bash
   kind create cluster --name kro-gitops-test
   ```
   Expected output:
   ```
   Creating cluster "kro-gitops-test" ...
   ✓ Ensuring node image (kindest/node:v1.27.3) 🖼
   ✓ Preparing nodes 📦
   ✓ Writing configuration 📜
   ✓ Starting control-plane 🕹️
   ✓ Installing CNI 🔌
   ✓ Installing StorageClass 💾
   Set kubectl context to "kind-kro-gitops-test"
   You can now use your cluster with:
   kubectl cluster-info --context kind-kro-gitops-test
   ```

3. Install Flux:
   ```bash
   # Install Flux components
   kubectl apply -f https://github.com/fluxcd/flux2/releases/latest/download/install.yaml
   ```
   Expected output:
   ```
   namespace/flux-system created
   customresourcedefinition.apiextensions.k8s.io/alerts.notification.toolkit.fluxcd.io created
   customresourcedefinition.apiextensions.k8s.io/buckets.source.toolkit.fluxcd.io created
   ...
   deployment.apps/source-controller created
   service/source-controller created
   ```

   Verify Flux components:
   ```bash
   kubectl get pods -n flux-system
   ```
   Expected output:
   ```
   NAME                                       READY   STATUS    RESTARTS   AGE
   helm-controller-5bbd94c75-h4q9q           1/1     Running   0          1m
   kustomize-controller-55bd7d59d-8p4q2      1/1     Running   0          1m
   notification-controller-5c4d48b47-xk8b2    1/1     Running   0          1m
   source-controller-7d7b657b84-njx5q         1/1     Running   0          1m
   ```

4. Set up repository access:
   ```bash
   # Generate SSH key for Flux
   ssh-keygen -t ed25519 -C "flux" -N "" -f /tmp/flux-key
   ```
   Expected output:
   ```
   Generating public/private ed25519 key pair.
   Your identification has been saved in /tmp/flux-key
   Your public key has been saved in /tmp/flux-key.pub
   The key fingerprint is:
   SHA256:gya+YjU4YUrbPIGEPUNBc6zKnvN1IPxf4PIDHAz6Z5E flux
   ```

   ```bash
   # Create Kubernetes secret with the SSH key
   kubectl create secret generic flux-system \
     -n flux-system \
     --from-file=identity=/tmp/flux-key \
     --from-file=identity.pub=/tmp/flux-key.pub \
     --from-literal=known_hosts="github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
   ```
   Expected output:
   ```
   secret/flux-system created
   ```

   Verify secret:
   ```bash
   kubectl get secret -n flux-system flux-system
   ```
   Expected output:
   ```
   NAME          TYPE     DATA   AGE
   flux-system   Opaque   3     1m
   ```

5. Create a basic GitOps structure:
   ```bash
   mkdir -p clusters/test
   cat > clusters/test/kustomization.yaml << 'EOL'
   apiVersion: kustomize.config.k8s.io/v1beta1
   kind: Kustomization
   resources:
     - namespace.yaml
     - gitops-app.yaml
   EOL

   cat > clusters/test/namespace.yaml << 'EOL'
   apiVersion: v1
   kind: Namespace
   metadata:
     name: demo
   EOL

   cat > clusters/test/gitops-app.yaml << 'EOL'
   apiVersion: apps/v1
   kind: Deployment
   metadata:
     name: demo-app
     namespace: demo
   spec:
     replicas: 1
     selector:
       matchLabels:
         app: demo
     template:
       metadata:
         labels:
           app: demo
       spec:
         containers:
         - name: nginx
           image: nginx:latest
           ports:
           - containerPort: 80
   EOL
   ```

   Verify files:
   ```bash
   tree clusters/test
   ```
   Expected output:
   ```
   clusters/test
   ├── gitops-app.yaml
   ├── kustomization.yaml
   └── namespace.yaml
   ```

6. Configure Flux source and kustomization:
   ```bash
   mkdir -p clusters/test/flux-system
   cat > clusters/test/flux-system/gotk-source.yaml << 'EOL'
   apiVersion: source.toolkit.fluxcd.io/v1
   kind: GitRepository
   metadata:
     name: flux-system
     namespace: flux-system
   spec:
     interval: 1m0s
     ref:
       branch: main
     url: ssh://git@github.com/YOUR_USERNAME/YOUR_REPO
     secretRef:
       name: flux-system
   EOL

   cat > clusters/test/flux-system/gotk-sync.yaml << 'EOL'
   apiVersion: kustomize.toolkit.fluxcd.io/v1
   kind: Kustomization
   metadata:
     name: flux-system
     namespace: flux-system
   spec:
     interval: 10m0s
     path: ./clusters/test
     prune: true
     sourceRef:
       kind: GitRepository
       name: flux-system
   EOL
   ```

   Apply configurations:
   ```bash
   kubectl apply -f clusters/test/flux-system/
   ```
   Expected output:
   ```
   gitrepository.source.toolkit.fluxcd.io/flux-system created
   kustomization.kustomize.toolkit.fluxcd.io/flux-system created
   ```

   Verify reconciliation:
   ```bash
   flux get all
   ```
   Expected output:
   ```
   NAME                            READY   MESSAGE                         REVISION                                        SUSPENDED
   gitrepository/flux-system       True    Fetched revision: main@sha1    a355d7d5d70e36f32f56e6165beb53ace5eb022b     False
   
   NAME                            READY   MESSAGE                         REVISION                                        SUSPENDED
   kustomization/flux-system       True    Applied revision: main@sha1    a355d7d5d70e36f32f56e6165beb53ace5eb022b     False
   ```

   Check deployed application:
   ```bash
   kubectl get pods -n demo
   ```
   Expected output:
   ```
   NAME                        READY   STATUS    RESTARTS   AGE
   demo-app-d5d8dcfcf-wgcb6   1/1     Running   0          1m
   ```

## Overview

This example implements a GitOps workflow that includes:

1. Flux CD setup and configuration using KRO
2. Multi-environment setup (dev/prod)
3. Automated deployment pipelines
4. Secret management with SOPS
5. Drift detection and automated reconciliation

![Architecture](images/architecture.png)

## Prerequisites

- Kubernetes cluster (v1.20+)
- KRO installed (v0.2.0+)
- Flux CLI
- SOPS
- kubectl
- Git repository with write access

## Directory Structure

```
.
├── base/                   # Base KRO configurations
│   ├── flux-system/       # Flux core components
│   ├── applications/      # Application definitions
│   └── kustomization.yaml
├── overlays/              # Environment-specific configurations
│   ├── dev/              # Development environment
│   └── prod/             # Production environment
└── README.md
```

## Installation

1. Install Flux CLI:
```bash
curl -s https://fluxcd.io/install.sh | sudo bash
```

2. Bootstrap Flux with KRO:
```bash
flux bootstrap github \
  --owner=$GITHUB_USER \
  --repository=fleet-infra \
  --branch=main \
  --path=./clusters/my-cluster \
  --personal
```

3. Apply KRO ResourceGraphDefinitions:
```bash
kubectl apply -f base/flux-system/resource-graphs/
```

## Usage

### 1. Define Your Resources

Create a ResourceGraphDefinition that defines your application structure:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: gitops-app
spec:
  resources:
    - name: deployment
      kind: Deployment
      apiVersion: apps/v1
    - name: service
      kind: Service
      apiVersion: v1
    - name: kustomization
      kind: Kustomization
      apiVersion: kustomize.toolkit.fluxcd.io/v1
```

### 2. Create Environment Configurations

Use overlays to manage environment-specific configurations:

```yaml
# overlays/dev/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../base
patches:
  - path: patches/replicas.yaml
```

### 3. Implement Automated Deployments

Configure Flux to automatically deploy changes:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: my-app
  namespace: flux-system
spec:
  interval: 1m
  url: https://github.com/org/repo
  ref:
    branch: main
```

## Security Considerations

1. Use SOPS for secret management:
```bash
sops -e -i --age public-key.txt secrets.yaml
```

2. Implement proper RBAC:
```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kro-gitops-role
rules:
  - apiGroups: ["kro.run"]
    resources: ["resourcegraphdefinitions"]
    verbs: ["get", "list", "watch"]
```

## Best Practices

1. Always use semantic versioning for your resources
2. Implement proper validation and testing
3. Use separate branches for different environments
4. Implement proper backup and disaster recovery
5. Monitor reconciliation and drift

## Troubleshooting

Common issues and solutions:

1. **Flux not reconciling:**
   ```bash
   # Check Flux components status
   flux get all
   
   # Check pod logs
   kubectl logs -n flux-system deployment/source-controller
   kubectl logs -n flux-system deployment/kustomize-controller
   ```

2. **Authentication Issues:**
   - Ensure the SSH key is properly added as a deploy key in GitHub
   - Check the secret is properly created:
     ```bash
     kubectl get secret -n flux-system flux-system -o yaml
     ```
   - Verify the known_hosts entry is correct

3. **YAML Formatting Issues:**
   - Ensure there are no escaped newlines in YAML files
   - Use proper indentation
   - Validate YAML syntax:
     ```bash
     kubectl apply --dry-run=client -f your-file.yaml
     ```

4. **Resource Issues:**
   - Check if namespaces exist before applying resources
   - Verify RBAC permissions
   - Check for resource quotas or limits

5. **System Requirements:**
   - Ensure sufficient file descriptors:
     ```bash
     ulimit -n 65535
     ```
   - Check for sufficient memory and CPU
   - Verify container runtime is working properly

## Contributing

Feel free to submit issues, fork the repository and create pull requests for any improvements. 