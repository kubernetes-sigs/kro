---
sidebar_position: 1
---

# Installing Symphony

This guide walks you through the process of installing Symphony on your
Kubernetes cluster using Helm.

## Prerequisites

Before you begin, ensure you have the following:

1. Access to an AWS account
2. AWS CLI installed and configured
3. Helm 3.x installed
4. kubectl installed and configured to interact with your Kubernetes cluster
5. Necessary permissions to pull images from Amazon ECR

## Installation Steps

:::info[**Getting Alpha Access**]
If you are reading this, you are installing a pre-alpha version of symphony,
that requires special authorization from the EKS service team. If you believe
that you don't have the necessary permissions, please reach out to Lukonde Mwila
lukondef@amazon.com
:::

### 1. Authenticate with Amazon ECR

First, authenticate your Helm client with the Amazon Elastic Container Registry
(ECR) that hosts the Symphony pre-alpha chart. Run the following command:

```sh
aws ecr get-login-password --region us-west-2 | helm registry login \
  --username AWS --password-stdin 095708837592.dkr.ecr.us-west-2.amazonaws.com
```

### 2. Install Symphony using Helm

Once authenticated, install Symphony using the Helm chart:

```sh
export SYMPHONY_VERSION=0.0.7 # TODO(a-hilaly): some curl-github-fu to get the latest version

helm install --version=${SYMPHONY_VERSION} -n symphony \
   symphony oci://095708837592.dkr.ecr.us-west-2.amazonaws.com/symphony-chart \
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
export SYMPHONY_VERSION=<new-version>

helm upgrade -n symphony \
  symphony oci://095708837592.dkr.ecr.us-west-2.amazonaws.com/symphony-chart \
  --version=${SYMPHONY_VERSION}
```

Replace `<new-version>` with the version you want to upgrade to.

:::info
Helm does not support updating CRDs, so you may need to manually update or remove
symphony related CRDs. For more information, refer to the Helm documentation.
:::


## Uninstalling Symphony

To uninstall Symphony, use the following command:

```bash
helm uninstall symphony
```

Keep in mind that this command will remove all Symphony resources from your
cluster, except for the ResourceGroup CRD and any other custom resources you
may have created.