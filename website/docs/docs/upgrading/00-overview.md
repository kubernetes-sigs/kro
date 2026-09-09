---
sidebar_position: 1
sidebar_label: Overview
---

# Upgrading kro

kro is upgraded by installing a newer chart or manifest over the existing
release. Most upgrades need nothing beyond the steps in
[Installation](../getting-started/01-Installation.md#upgrade). This section
lists the releases that need more than that: a manual step before or after the
upgrade, or a change that can make an existing ResourceGraphDefinition behave
differently.

## Custom Resource Definitions

Helm does not install new CRDs or update existing ones on `helm upgrade`. When a
release adds or changes a CRD, apply the CRDs from that release before upgrading
the controller:

```bash
export KRO_VERSION=<version>
kubectl apply --server-side -f https://raw.githubusercontent.com/kubernetes-sigs/kro/v${KRO_VERSION}/helm/crds/kro.run_resourcegraphdefinitions.yaml
kubectl apply --server-side -f https://raw.githubusercontent.com/kubernetes-sigs/kro/v${KRO_VERSION}/helm/crds/internal.kro.run_graphrevisions.yaml
kubectl apply --server-side -f https://raw.githubusercontent.com/kubernetes-sigs/kro/v${KRO_VERSION}/helm/crds/kro.run_graphs.yaml
```

The raw manifest install (`kro-core-install-manifests.yaml`) includes the CRDs,
so `kubectl apply` of the new manifest updates them.

## Release Notes

- **[v0.10.0](./01-v0.10.0.md)** - New `Graph` API, unified composition engine, stricter expression parsing
