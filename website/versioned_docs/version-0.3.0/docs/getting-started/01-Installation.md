---
sidebar_position: 1
---

# Installing kro

This guide walks you through the process of installing kro on your Kubernetes
cluster using Helm.

## Prerequisites

Before you begin, ensure you have the following:

1. `Helm` 3.x installed
2. `kubectl` installed and configured to interact with your Kubernetes cluster

## Installation Steps

:::info[**Alpha Stage**]

kro is currently in alpha stage. While the images are publicly available, please
note that the software is still under active development and APIs may change.

:::

### Install kro using Helm

Once authenticated, install kro using the Helm chart:

Fetch the latest release version from GitHub
```sh
export KRO_VERSION=$(curl -sL \
    https://api.github.com/repos/kro-run/kro/releases/latest | \
    jq -r '.tag_name | ltrimstr("v")'
  )
```
Validate `KRO_VERSION` populated with a version
```
echo $KRO_VERSION
```
Install kro using Helm
```
helm install kro oci://ghcr.io/kro-run/kro/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}
```

:::info[**Troubleshooting Helm Install**]
Note that authentication is not required for pulling charts from public GHCR (GitHub Container Registry) repositories.

Helm install download failures occur due to expired local credentials. To resolve this issue, clear your local credentials cache by running `helm registry logout ghcr.io` in your terminal, then retry the installation.

:::

## Verifying the Installation

After running the installation command, verify that Kro has been installed
correctly:

1. Check the Helm release:

   ```sh
   helm -n kro list
   ```

   Expected result: You should see the "kro" release listed.
   ```
    NAME	NAMESPACE	REVISION	STATUS  
    kro 	kro      	1       	deployed
   ```

2. Check the kro pods:
   ```sh
   kubectl get pods -n kro
   ```
   Expected result: You should see kro-related pods running.
   ```
    NAME                        READY   STATUS             RESTARTS   AGE
    kro-7d98bc6f46-jvjl5        1/1     Running            0           1s 
   ```

## Upgrading kro

To upgrade to a newer version of kro, use the Helm upgrade command:

Replace `<new-version>` with the version you want to upgrade to.
```bash
export KRO_VERSION=<new-version>
```

Upgrade the controller
```
helm upgrade kro oci://ghcr.io/kro-run/kro/kro \
  --namespace kro \
  --version=${KRO_VERSION}
```

:::info[**CRD Updates**]

Helm does not support updating CRDs, so you may need to manually update or
remove and re-apply kro related CRDs. For more information, refer to the Helm
documentation.

:::

## Uninstalling kro

To uninstall kro, use the following command:

```bash
helm uninstall kro -n kro
```

Keep in mind that this command will remove all kro resources from your cluster,
except for the ResourceGraphDefinition CRD and any other custom resources you may have
created.
