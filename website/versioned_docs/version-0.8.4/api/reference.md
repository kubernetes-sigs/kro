---
sidebar_position: 0
sidebar_label: Reference
---

# Reference

Reference documentation for kro's APIs and specifications.

## CRDs

Custom Resource Definitions (CRDs) that extend Kubernetes with kro's functionality.

- **[ResourceGraphDefinition](crds/resourcegraphdefinition.md)** - The core API for defining custom Kubernetes resources that orchestrate multiple underlying resources. Define your resource schemas, specify resource dependencies, and create reusable infrastructure patterns.

## Specifications

Schema and language specifications used when authoring kro resources.

- **[SimpleSchema](specifications/simple-schema.md)** - A concise schema definition language for defining resource structures in ResourceGraphDefinitions without writing verbose OpenAPI schemas.
