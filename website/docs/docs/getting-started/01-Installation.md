---
sidebar_position: 1
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Installing kro

Install kro on your Kubernetes cluster using Helm.

## Prerequisites

- Helm 3.x installed
- kubectl configured to access your cluster

## Installation

<Tabs>
  <TabItem value="quickstart" label="Quick Start" default>

Install the latest version:

```bash
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --create-namespace
```

  </TabItem>
  <TabItem value="pinned" label="Pinned Version">

Install a specific version for reproducible deployments:

```bash
# Set the version you want to install
export KRO_VERSION=0.6.2

# Install kro
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}
```

Or automatically use the latest release:

```bash
# Fetch latest release version
export KRO_VERSION=$(curl -sL \
    https://api.github.com/repos/kubernetes-sigs/kro/releases/latest | \
    jq -r '.tag_name | ltrimstr("v")')

# Verify version
echo $KRO_VERSION

# Install kro
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}
```

  </TabItem>
</Tabs>

## Verify Installation

Check the Helm release:

```bash
helm list -n kro
```

Expected output:
```
NAME  NAMESPACE  REVISION  STATUS
kro   kro        1         deployed
```

Check the kro pod is running:

```bash
kubectl get pods -n kro
```

Expected output:
```
NAME                   READY   STATUS    RESTARTS   AGE
kro-7d98bc6f46-jvjl5   1/1     Running   0          30s
```

## Upgrade

<Tabs>
  <TabItem value="latest" label="Latest Version" default>

```bash
helm upgrade kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro
```

  </TabItem>
  <TabItem value="specific" label="Specific Version">

```bash
export KRO_VERSION=0.6.2

helm upgrade kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --version=${KRO_VERSION}
```

  </TabItem>
</Tabs>

:::info
Helm does not update CRDs automatically. If a new version includes CRD changes, you may need to manually apply them. Check the release notes for CRD updates.
:::

## Uninstall

```bash
helm uninstall kro -n kro
```

:::info
This removes the kro controller but preserves your RGDs and deployed instances. To fully clean up, manually delete instances and RGDs before uninstalling.
:::
