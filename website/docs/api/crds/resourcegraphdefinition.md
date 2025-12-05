---
sidebar_position: 1
sidebar_label: ResourceGraphDefinition
hide_breadcrumbs: true
hide_table_of_contents: true
---

import RGDReference from '@site/src/components/RGDReference';

<head>
  <html className="fullWidthContent" />
</head>

# ResourceGraphDefinition

<div style={{fontSize: '1.2em', marginBottom: '2rem', color: 'var(--ifm-color-emphasis-700)', lineHeight: '1.6'}}>
The ResourceGraphDefinition (RGD) is kro's core API for defining custom Kubernetes resources that orchestrate multiple underlying resources.
</div>

---

## API Specification

<div style={{display: 'flex', gap: '1rem', marginBottom: '3rem', flexWrap: 'wrap'}}>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>API Version</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>kro.run/v1alpha1</code>
  </div>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>Kind</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>ResourceGraphDefinition</code>
  </div>
  <div style={{flex: '1', minWidth: '200px', padding: '1.5rem', border: '1px solid var(--ifm-color-emphasis-200)', borderRadius: '12px', background: 'var(--ifm-background-color)'}}>
    <div style={{fontSize: '0.85em', color: 'var(--ifm-color-emphasis-600)', marginBottom: '0.5rem', fontWeight: '600'}}>Scope</div>
    <code style={{fontSize: '1.1em', color: 'var(--ifm-color-primary)', fontWeight: '600'}}>Cluster</code>
  </div>
</div>

---

## Fields Reference

<RGDReference />
