---
sidebar_position: 1
---

# Installing KRO

This guide walks you through the process of installing KRO on your Kubernetes
cluster using Helm.

## Prerequisites

Before you begin, ensure you have the following:

1. `Helm` 3.x installed
2. `kubectl` installed and configured to interact with your Kubernetes cluster

## Installation Steps

:::info[**Alpha Stage**]

KRO is currently in alpha stage. While the images are publicly available, please
note that the software is still under active development and APIs may change.

:::

### Install KRO using Helm

Once authenticated, install KRO using the Helm chart:

```sh
# Fetch the latest release version from GitHub
export KRO_VERSION=$(curl -s \
    https://api.github.com/repos/awslabs/kro/releases/latest | \
    grep '"tag_name":' | \
    sed -E 's/.*"([^"]+)".*/\1/' \
  )

# Install KRO using Helm
helm install kro oci://public.ecr.aws/kro/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}
```

## Verifying the Installation

After running the installation command, verify that Kro has been installed
correctly:

1. Check the Helm release:

   ```sh
   helm list
   ```

   You should see the "kro" release listed.

2. Check the KRO pods:
   ```sh
   kubectl get pods -n kro
   ```
   You should see kro-related pods running.

## Upgrading KRO

To upgrade to a newer version of KRO, use the Helm upgrade command:

```bash
# Replace `<new-version>` with the version you want to upgrade to.
export KRO_VERSION=<new-version>

# Upgrade the controller
helm upgrade kro oci://public.ecr.aws/kro/kro \
  --namespace kro \
  --version=${KRO_VERSION}
```

:::info[**CRD Updates**]

Helm does not support updating CRDs, so you may need to manually update or
remove and re-apply kro related CRDs. For more information, refer to the Helm documentation.

:::

## Uninstalling KRO

To uninstall KRO, use the following command:

```bash
helm uninstall kro -n kro
```

Keep in mind that this command will remove all KRO resources from your cluster,
except for the ResourceGroup CRD and any other custom resources you may have
created.
