---
sidebar_position: 1
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# Installing kro

Install kro on your Kubernetes cluster using Helm.

## Prerequisites

Kro can be installed via Helm or raw manifests. Pick the solution that best fits you.

- Helm 3.x installed (only for helm installation)
- kubectl configured to access your cluster

## Installation

<Tabs>
  <TabItem value="quickstart" label="Quick Start" default>

Install the latest version:

```bash
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro-system \
  --create-namespace
```

  </TabItem>
  <TabItem value="pinned" label="Pinned Version">

Install a specific version for reproducible deployments:

```bash
# Set the version you want to install
export KRO_VERSION=0.8.0

# Install kro
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro-system \
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
  --namespace kro-system \
  --create-namespace \
  --version=${KRO_VERSION}
```

  </TabItem>
  <TabItem value="kubectl" label="Raw manifest installation" default>

Select which pre-built flavour you would like to install

| variant                                    | Prometheus scrape config |
| ------------------------------------------ | ------------------------ |
| kro-core-install-manifests                 | Not installed            |
| kro-core-install-manifests-with-prometheus | Installed                |

Fetch the latest release version from GitHub

```sh
export KRO_VERSION=$(curl -sL \
      https://api.github.com/repos/kubernetes-sigs/kro/releases/latest | \
      jq -r '.tag_name | ltrimstr("v")'
   )
export KRO_VARIANT=kro-core-install-manifests

echo $KRO_VERSION
```

```bash
kubectl create namespace kro-system
kubectl apply -f https://github.com/kubernetes-sigs/kro/releases/download/v$KRO_VERSION/$KRO_VARIANT.yaml
```
  </TabItem>
</Tabs>

## Verify Installation

<Tabs>
  <TabItem value="helm" label="Helm installation" default>
Check the Helm release:

```bash
helm list -n kro-system
```


Expected output:
```
NAME  NAMESPACE  REVISION  STATUS
kro   kro-system 1         deployed
```

Check the kro pod is running:

```bash
kubectl get pods -n kro-system
```

Expected output:
```
NAME                   READY   STATUS    RESTARTS   AGE
kro-7d98bc6f46-jvjl5   1/1     Running   0          30s
```
  </TabItem>
  <TabItem value="kubectl" label="Raw manifest installation">
Check the Helm release:

```bash

Check the kro pod is running:

```bash
kubectl get pods -n kro-system
```

Expected output:
```
NAME                   READY   STATUS    RESTARTS   AGE
kro-7d98bc6f46-jvjl5   1/1     Running   0          30s
```
  </TabItem>
</Tabs>

## Upgrade

<Tabs>
  <TabItem value="latest" label="Latest Version" default>

```bash
helm upgrade kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro-system
```

  </TabItem>
  <TabItem value="specific" label="Specific Version">

```bash
export KRO_VERSION=0.8.0

helm upgrade kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro-system \
  --version=${KRO_VERSION}
```

:::info
Helm does not update CRDs automatically. If a new version includes CRD changes, you may need to manually apply them. Check the release notes for CRD updates.
:::
  </TabItem>
  <TabItem value="kubectl" label="Raw manifest installation">
```
export KRO_VERSION=$(curl -sL \
      https://api.github.com/repos/kubernetes-sigs/kro/releases/latest | \
      jq -r '.tag_name | ltrimstr("v")'
   )
export KRO_VARIANT=kro-core-install-manifests
```

```
kubectl apply -f https://github.com/kubernetes-sigs/kro/releases/download/v$KRO_VERSION/$KRO_VARIANT.yaml
```

:::info[**Removal of dangling objects**]

Kubectl installation does remove old objects. You may need to manually remove any
object that was removed in the new version of Kro.
:::

  </TabItem>
</Tabs>

## Uninstall


<Tabs>
  <TabItem value="helm" label="Helm installation" default>
```bash
helm uninstall kro -n kro-system
```
  </TabItem>
  <TabItem value="kubectl" label="Raw manifest installation">
```bash
kubectl delete -f https://github.com/kubernetes-sigs/kro/releases/download/v$KRO_VERSION/$KRO_VARIANT.yaml
```
  </TabItem>
</Tabs>

:::info
This removes the kro controller but preserves your RGDs and deployed instances. To fully clean up, manually delete instances and RGDs before uninstalling.
:::
