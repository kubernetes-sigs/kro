---
sidebar_position: 0
---

# Examples

This section provides a collection of examples demonstrating how to define and
use ResourceGraphDefinitions in **kro** for various scenarios. Each example showcases a
specific use case and includes a detailed explanation along with the
corresponding YAML definitions.

## Basic Examples

- [Empty ResourceGraphDefinition (Noop)](./noop.md) Explore the simplest form of a
  ResourceGraphDefinition that doesn't define any resources, serving as a reference for
  the basic structure.

- [Simple Web Application](./web-app.md) Deploy a basic web application with a
  Deployment and Service.

- [Web Application with Ingress](./web-app-ingress.md) Extend the basic web
  application example to include an optional Ingress resource for external
  access.

## Advanced Examples

- [Deploying CoreDNS](./deploying-coredns.md) Learn how to deploy CoreDNS in a
  Kubernetes cluster using kro ResourceGraphDefinitions, including the necessary
  Deployment, Service, and ConfigMap.

- [Deploying a Controller](./deploying-controller.md) Discover how to deploy a
  Kubernetes controller using kro ResourceGraphDefinitions, including the necessary
  Deployment, ServiceAccount, and CRDs.

- [AWS Networking Stack](./ack-networking-stack.md) Learn how to define and
  manage an AWS networking stack using kro ResourceGraphDefinitions, including VPCs,
  subnets, and security groups.

- [EKS Cluster with ACK CRDs](./ack-eks-cluster.md) Explore how to define and
  manage an EKS cluster using AWS Controllers for Kubernetes (ACK) CRDs within a
  kro ResourceGraphDefinition.

- [Valkey CacheCluster with ACK CRDs](./ack-valkey-cachecluster.md) Learn how to
  create and configure a Valkey CacheCluster using ACK CRDs in a kro
  ResourceGraphDefinition.

- [Pod and RDS DBInstance](./pod-rds-dbinstance.md) Deploy a Pod and an RDS
  DBInstance in a kro ResourceGraphDefinition, showcasing the use of multiple resources
  with dependencies.
