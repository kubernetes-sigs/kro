---
sidebar_position: 0
---

# Examples

This section provides a collection of examples demonstrating how to define and
use ResourceGraphDefinitions and Graphs in **kro** for various scenarios. Each
example showcases a specific use case and includes a detailed explanation along
with the corresponding YAML definitions.

## Basic Examples

- [Empty ResourceGraphDefinition (Noop)](./basic/noop.md) Explore the simplest form of a
  ResourceGraphDefinition that doesn't define any resources, serving as a reference for
  the basic structure.

- [Simple Web Application](./basic/web-app.md) Deploy a basic web application with a
  Deployment and Service.

- [Web Application with Ingress](./basic/web-app-ingress.md) Extend the basic web
  application example to include an optional Ingress resource for external
  access.

## Advanced Examples

- [Deploying CoreDNS](./kubernetes/deploying-coredns.md) Learn how to deploy CoreDNS in a
  Kubernetes cluster using kro ResourceGraphDefinitions, including the necessary
  Deployment, Service, and ConfigMap.

- [SaaS Multi-Tenant](./kubernetes/saas-multi-tenant.md) This example demonstrates
  how to create a multi-tenant SaaS application using Kro ResourceGraphDefinitions.
  It creates isolated tenant environments with dedicated applications,
  following a hierarchical structure of ResourceGraphDefinitions.

## Graph Examples

The [Graph](../docs/concepts/graph/01-overview.md) API is alpha and disabled by
default. These examples show the patterns it enables.

- [Namespace Decorator](./graph/namespace-decorator.md) Watch labeled Namespaces
  and create a default-deny NetworkPolicy in each one, with no CRD or instance.

- [Ingress Fan-In](./graph/ingress-fanin.md) Aggregate every labeled Service into
  a single Ingress with one rule per Service.

- [CoreDNS Bundle](./graph/coredns.md) Install CoreDNS as one dependency-ordered,
  health-checked object.

- [Singleton Controller](./graph/singleton.md) A small controller written as a
  Graph: priority-based ownership of a shared resource, with status written back
  to every claimant.
