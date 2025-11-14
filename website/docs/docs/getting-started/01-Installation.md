---
sidebar_position: 1
---

# Installing kro

This guide walks you through the process of installing kro on your Kubernetes
cluster using Helm.

## Prerequisites

:::info[**Alpha Stage**]

kro is currently in alpha stage. While the images are publicly available, please
note that the software is still under active development and APIs may change.

:::

Before you begin, ensure you have the following:

1. `Helm` 3.x installed (for helm based installation)
2. `kubectl` installed and configured to interact with your Kubernetes cluster
3. chose the version you want to install or fetch the latest release from GitHub
   Fetch the latest release version from GitHub
   ```sh
   export KRO_VERSION=$(curl -sL \
       https://api.github.com/repos/kubernetes-sigs/kro/releases/latest | \
       jq -r '.tag_name | ltrimstr("v")'
     )
   ```
   Validate `KRO_VERSION` populated with a version
   ```
   echo $KRO_VERSION
   ```
4. For kubectl installation, select the variant you want to install
   | variant                                    | Prometheus scrape config | Kro permissions |
   | ------------------------------------------ | ------------------------ | --------------- |
   | kro-core-install-manifests                 | Not installed            | least privilege |
   | kro-core-install-manifests-with-prometheus | Installed                | least privilege |
   | kro-unrestricted-manifests                 | Not installed            | cluster admin   |
   | kro-unrestricted-manifests-with-prometheus | Installed                | cluster admin   |

## Installation with helm

### Installation Steps

#### Install kro using Helm

Once authenticated, install kro using the Helm chart:

Install kro using Helm
```
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --create-namespace \
  --version=${KRO_VERSION}
```

### Verifying the Installation

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

### Upgrading kro

To upgrade to a newer version of kro, use the Helm upgrade command:

Replace `<new-version>` with the version you want to upgrade to.
```bash
export KRO_VERSION=<new-version>
```

Upgrade the controller
```
helm upgrade kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro \
  --version=${KRO_VERSION}
```

:::info[**CRD Updates**]

Helm does not support updating CRDs, so you may need to manually update or
remove and reapply kro related CRDs. For more information, refer to the Helm
documentation.

:::

### Uninstalling kro

To uninstall kro, use the following command:

```bash
helm uninstall kro -n kro
```

Keep in mind that this command will remove all kro resources from your cluster,
except for the ResourceGraphDefinition CRD and any other custom resources you may have
created.

## Installation with kubectl

### Installation Steps

#### Install kro using Kubectl

Select the variant you want (see available variants in [prerequisites](#prerequisites))
```
export KRO_VARIANT=kro-core-install-manifests
```

Install kro using Kubectl
```
kubectl apply -f https://github.com/kubernetes-sigs/kro/releases/download/$KRO_VERSION/$KRO_VARIANT.yaml
```

### Verifying the Installation

After running the installation command, verify that Kro has been installed
correctly:

Check the kro pods:
```sh
kubectl get pods -n kro-system
```
Expected result: You should see kro-related pods running.
```
NAME                        READY   STATUS             RESTARTS   AGE
kro-7d98bc6f46-jvjl5        1/1     Running            0           1s 
```

### Upgrading kro

To upgrade to a newer version of kro, use the Helm upgrade command:

Replace `<new-version>` with the version you want to upgrade to.
```bash
export KRO_VERSION=<new-version>
```

Upgrade the controller
```
kubectl apply -f https://github.com/kubernetes-sigs/kro/releases/download/$KRO_VERSION/$KRO_VARIANT.yaml
```

:::info[**Removal of dangling objects**]

Kubectl installation does remove old objects. You may need to manually remove any
object that was removed in the new version of Kro.

:::

### Uninstalling kro

To uninstall kro, use the following command:

```bash
kubectl delete -f https://github.com/kubernetes-sigs/kro/releases/download/$KRO_VERSION/$KRO_VARIANT.yaml
```

Keep in mind that this command will remove all kro resources from your cluster,
including the ResourceGraphDefinition CRD. This means all RGD instances will also be deleted.
Resources deployed by RGD instances may though be kept in the cluster depending whether the kro controller
is deleted before of after the ResourceGraphDefinition CRD.