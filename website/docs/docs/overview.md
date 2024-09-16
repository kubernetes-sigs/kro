---
sidebar_position: 1
---

# Overview

Symphony is a **Kubernetes controller** designed to simplify
and streamline resource management in Kubernetes environments,
in a **safe** and **declarative** way.

Symphony acts as a composition layer on top of Kubernetes, allowing
users to define high-level **abstractions** of interconnected
resources. Users can then instantiate these abstractions, which Symphony
translates and manages as actual Kubernetes resources.

It is capable of understanding the **dependencies** between
resources and orchestrating their creation, update, and deletion
in a declarative way.

Symphony is designed to be a flexible and extensible tool that can
work with any Kubernetes cluster, allowing users to define orchestrate
both **native** and **custom resources**.

Symphony enables the creation of powerful, reusable abstractions.
Users can define complex, multi-resource abstractions that encapsulate
best practices and organizational standards. These abstractions can
be easily shared and reused across teams and projects, promoting
consistency and accelerating development cycles.

## Key Benefits

- **Simplicity**: Symphony makes it easy for anyone to declare
  abstractions, regardless of their expertise level with Kubernetes.
  The intuitive interface allows for super simple definition of complex
  resource structures.

- **Flexibility**: Symphony works seamlessly with both native Kubernetes
  resources and custom resource definitions, providing a unified approach
  to resource management across your entire cluster.

- **Predictability**: Before actual deployment, Symphony can perform dry
  runs and type checking for your resources, allowing you to predict the
  results of your deployment accurately. This feature helps catch potential
  issues early in the development cycle.

- **Observability**: Symphony enhances your ability to monitor and understand
  your Kubernetes environment by providing comprehensive metrics, events, and
  conditions.

- **Familiar Expression Language**: Symphony leverages Common Expression
  Language (CEL), which is already used in Kubernetes for admission control
  and native functionality. This makes it easy for Kubernetes users to onboard
  and immediately start writing powerful, dynamic expressions for resource
  definitions, validations, and policies.

These benefits combine to make Symphony a powerful ally in managing Kubernetes
resources efficiently and effectively, regardless of the scale or complexity
of your applications.

## Use Cases

Symphony's powerful abstraction and composition capabilities make it
suitable for a wide range of Kubernetes use cases:

1. **Application deployment simplification:**
   Simplify complex application deployments by encapsulating all
   necessary resources (pods, services, config maps, etc.) into a
   single, easy-to-use abstraction.

2. **Multi environment management:**
   Create consistent deployments across different environments (dev,
   staging, prod), zones, and geographical regions. Define environment
   and location-specific parameters within a single abstraction,
   facilitating global application distribution and disaster recovery
   strategies.

4. **Infrastructure-as-Code standards enforcement:**
   Enforce organizational best practices and security standards by
   embedding them directly into reusable resource definitions.

5. **Platform-as-a-Service creation:**
   Build internal platforms that provide simplified, secure interfaces
   for developers to deploy applications without needing to understand
   the underlying Kubernetes complexity.

6. **Custom resource extension:**
   Extend Kubernetes native capabilities by creating high-level
   abstractions that combine multiple custom and native resources.

7. **Gitops workflow enhancement:**
   Improve GitOps workflows by working with high-level abstractions
   that are easier to version, review, and manage in source control.