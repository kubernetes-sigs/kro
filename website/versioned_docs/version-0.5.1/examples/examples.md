---
sidebar_position: 0
---

# Examples

This section provides a collection of examples demonstrating how to define and
use ResourceGraphDefinitions in **kro** for various scenarios. Each example showcases a
specific use case and includes a detailed explanation along with the
corresponding YAML definitions.

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
