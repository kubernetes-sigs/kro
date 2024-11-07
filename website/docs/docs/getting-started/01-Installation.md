---
sidebar_position: 1
---

# Installing Symphony

This guide walks you through the process of installing Symphony on your
Kubernetes cluster using Helm.

## Prerequisites

Before you begin, ensure you have the following:

1. `Helm` 3.x installed
2. `kubectl` installed and configured to interact with your Kubernetes cluster

## Installation Steps

:::info[**Alpha Stage**] Symphony is currently in alpha stage. While the images
are publicly available, please note that the software is still under active
development and APIs may change. :::

### Install Symphony using Helm

Once authenticated, install Symphony using the Helm chart:

```sh
# Fetch the latest release version from GitHub
export SYMPHONY_VERSION=$(curl -s \
    https://api.github.com/repos/awslabs/private-symphony/releases/latest | \
    grep '"tag_name":' | \
    sed -E 's/.*"([^"]+)".*/\1/' \
  )

# Install Symphony
helm install symphony oci://public.ecr.aws/symphony/symphony \
  --namespace symphony \
  --create-namespace \
  --version=${SYMPHONY_VERSION}
```

## Verifying the Installation

After running the installation command, verify that Symphony has been installed
correctly:

1. Check the Helm release:

   ```sh
   helm list
   ```

   You should see the "symphony" release listed.

2. Check the Symphony pods:
   ```sh
   kubectl get pods
   ```
   You should see Symphony-related pods running.

## Upgrading Symphony

To upgrade to a newer version of Symphony, use the Helm upgrade command:

```bash
# Replace `<new-version>` with the version you want to upgrade to.
export SYMPHONY_VERSION=<new-version>

# Upgrade the controller
helm upgrade symphony oci://public.ecr.aws/symphony/symphony \
  --namespace symphony \
  --version=${SYMPHONY_VERSION}
```

:::info Helm does not support updating CRDs, so you may need to manually update
or remove symphony related CRDs. For more information, refer to the Helm
documentation. :::

## Uninstalling Symphony

To uninstall Symphony, use the following command:

```bash
helm uninstall symphony
```

Keep in mind that this command will remove all Symphony resources from your
cluster, except for the ResourceGroup CRD and any other custom resources you may
have created.
