# KRO GitOps with Flux CD Example

This example demonstrates how to implement GitOps practices using KRO (Kubernetes Resource Orchestrator) with Flux CD. It shows how to manage Kubernetes resources using Git as the single source of truth while leveraging KRO's powerful resource orchestration capabilities.

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
   flux logs --level=error
   ```

2. **KRO resource issues:**
   ```bash
   kubectl describe resourcegraphdefinition my-app
   ```

## Contributing

Feel free to submit issues, fork the repository and create pull requests for any improvements. 