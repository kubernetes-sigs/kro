---
sidebar_position: 2
sidebar_label: Graph
hide_breadcrumbs: true
hide_table_of_contents: true
---

import RGDReference from '@site/src/components/RGDReference';
import crdYaml from './kro.run_graphs.yaml';

<head>
  <html className="fullWidthContent" />
</head>

# Graph

<div style={{fontSize: '1.2em', marginBottom: '2rem', color: 'var(--ifm-color-emphasis-700)', lineHeight: '1.6'}}>
A Graph is a namespaced set of nodes that create, read, and contribute to Kubernetes resources, connected by CEL expressions. kro reconciles it directly, without generating a new API.
</div>

:::warning Alpha Feature
The Graph API is alpha and disabled by default. It requires the `GraphKind`
feature gate. See the [Graph overview](../../docs/concepts/graph/01-overview.md).
:::

---

## API Specification

<div style={{display: 'flex', gap: '1rem', marginBottom: '3rem', flexWrap: 'wrap'}}>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>API Version</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>kro.run/v1alpha1</code>
  </div>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>Kind</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>Graph</code>
  </div>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>Scope</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>Namespaced</code>
  </div>
</div>

---

## Fields Reference

<RGDReference crdYaml={crdYaml} />
