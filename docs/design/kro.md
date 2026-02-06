# KREP-000: kro — Kube Resource Orchestrator

## Summary

kro lets you define custom Kubernetes APIs using declarative configuration. You describe a schema for your new API, the resources it should create and manage, and how data flows between them using CEL expressions. kro generates the CRD, builds the dependency graph, and reconciles instances continuously — the full custom controller pattern, without writing controller code.

This document is the foundational proposal for the kro project. It captures the problem kro solves, the approach it takes, and the principles that guide its design.

## Motivation

Kubernetes is extended through CRDs and custom controllers, leveraging the operator pattern. This pattern powers the broad CNCF ecosystem, and is the foundational construct upon which we build platforms with Kubernetes. The pattern is proven and powerful, but leveraging it requires writing, testing, and operating custom controller code for each new API.

As Kubernetes has become a universal control plane for all infrastructure, organizations need many custom APIs, often dozens crossing different infrastructure patterns. A database isn't just a cloud resource, it includes security groups, IAM roles, connection secrets, backup policies, and monitoring alerts, all with ordering constraints and data flow between them. An application stack may include that database, with a Deployment, a Service, an optional Ingress, and number of additional cloud resources mixed in.

To simplify onboarding and to maintain velocity for teams, platform builders often create custom APIs like this, composing multiple resources and components into consumable building blocks. Here too, the operator pattern is used to create 'glue controllers'. Writing bespoke controllers for these patterns is expensive, can be complex, and typically requires dedicated resource to maintain and manage at scale. We feel this is a missing feature of Kubernetes, and deserves a simple solution that other tools can leverage and build from.

### What's Missing

Existing tools each solve part of the problem but leave gaps:

**Templating tools** are client-side. They generate YAML but don't run in the cluster. They cannot evaluate inputs server-side, reference runtime data between resources, infer dependencies, or provide continuous reconciliation.

**Infrastructure-as-Code tools** are not Kubernetes-native. They require separate state management and induce a split-brain problem for resource management, they don't leverage Kubernetes primitives (RBAC, namespaces, reconciliation), and focus on imperative provisioning rather than declarative convergence from within the cluster.

**Composition frameworks** use patch-and-transform patterns that become complex for multi-resource graphs. They don't provide compile-time type checking of field references, don't automatically infer dependencies from references, and introduce their own provider abstraction layer rather than working directly with existing operators.

There is no streamlined, simple way to define custom APIs that orchestrate multiple resources using declarative configuration and native Kubernetes idioms, with automatic dependency inference, type-safe expressions, and in-cluster reconciliation.

## Proposal

### Overview

kro abstracts the Kubernetes operator pattern into declarative configuration. Rather than replacing native Kubernetes methods, it automates them for you. Instead of defining a CRD and implementing a controller, you define a ResourceGraphDefinition (RGD), which is a specification that describes your custom API and its implementation. From this specification, kro automatically creates your CRD and adapts to manage your new resources for you.

- **Custom API Schema** — The spec and status fields for your new API, along with constituent resources, are defined using SimpleSchema
- **Resource Orchestration** — Kubernetes resources of any kind (native types, installed CRDs, or custom resources defined with kro) are automatically created and managed
- **Data Flow** — CEL expressions are used to natively wire data between resources, enabling config injection and cross resource status reconciliation
- **Control Logic** — Conditional creation, dynamic collections, and custom readiness checks are in scope through native Kubernetes idioms
- **Versioning** — Immutable graph revisions track changes to definitions, enabling rollback and controlled propagation of changes to instances

When you create an RGD, kro validates it by type checking all CEL expressions against OpenAPI schemas, builds the dependency graph by analyzing CEL references, creates and installs the CRD, and starts reconciling instances. The result is a new Kubernetes API with continuous reconciliation, identical to what a hand-written glue controller would produce, in a fraction of the time and with none of the complexity and domain expertise required.

### Design Principles

**Kubernetes-native technologies.** kro builds on CEL (the same expression language used by ValidatingAdmissionPolicy and admission webhooks), OpenAPI schemas, server-side apply, standard status conditions, and RBAC. It feels like a natural extension of Kubernetes, not an external tool layered on top.

**Automatic dependency inference.** kro builds a directed acyclic graph by analyzing all CEL expressions. When one resource references another (`${database.status.endpoint}`), kro creates a dependency edge. Creation order, deletion order, and readiness propagation are all computed from the graph. No manual ordering required.

**Compile-time type safety.** CEL expressions are validated against OpenAPI schemas at RGD creation time. Type mismatches, undefined field references, and circular dependencies are caught before any instances are created.

**Works with any resource.** kro orchestrates native Kubernetes resources, CRDs from any operator (ACK, KCC, ASO, Crossplane), and resources defined by other RGDs. It does not introduce its own provider abstraction — it works with whatever is already in your cluster.

**Server-side reconciliation.** RGDs are reconciled continuously in the cluster, not applied once from a client. Drift is corrected, status is aggregated, and the full Kubernetes operational model applies.

### Core Concepts

**ResourceGraphDefinition (RGD)** — the single artifact that defines a custom API and its implementation. An RGD contains a schema (what users provide), resource templates (what gets created), and CEL expressions (how data flows). kro treats each RGD as a controller specification.

**SimpleSchema** — a concise, human-friendly syntax for defining custom API schemas that kro transforms into full OpenAPI v3 schemas. `replicas: integer | default=3 | min=1 | max=10` generates a proper OpenAPI schema used for CRD validation, kubectl explain, and CEL type checking.

**CEL Expressions** — `${expression}` syntax references user inputs (`${schema.spec.replicas}`), resource status (`${database.status.endpoint}`), metadata (`${service.metadata.name}`), and computed values (`${schema.spec.name + "-backup"}`). Expressions are validated against actual resource schemas at RGD creation time.

**Dependency Graph** — a DAG computed entirely from CEL references. If a Deployment references `${configMap.data.config}` and a Service references `${deployment.spec.selector}`, kro determines: configMap → deployment → service. No manual ordering. Each graph is versioned as an immutable revision, enabling change tracking and rollback.

### What kro Enables

Teams can define a set of building block resources that are custom-built for their needs. These custom resource can include any number of constituent resources, and their configuration can be exposed through the custom API or hard-wired, establishing organizational best practices as hard defaults. These resources can be authored by a central team, versioned, and distributed for use across an organization. This "prescriptive resource" pattern provides a means of defining your own simplified platform layer, with APIs as simple or as complex as you need them to be.

As an example, a platform team defines a `ManagedDatabase` RGD that accepts simple inputs (name, size, environment) and provisions a cloud database instance via installed controllers (ACK, KCC, ASO, etc.), creates security groups, generates IAM roles, produces a K8s Secret with connection details, and exposes the endpoint in status. Most of the configuration of these resources are baked into the resource definition, and a consumer doesn't need to worry about them. A resource may be as simple as:

```yaml
apiVersion: platform.mycompany.com/v1
kind: ManagedDatabase
metadata:
  name: myapp-db
spec:
  size: medium
  environment: production
```

kro handles dependency ordering, data flow, underlying resource configuration, status aggregation, and continuous reconciliation. The developer gets a Kubernetes-native API that works with kubectl, RBAC, namespaces, and GitOps tools.

This pattern scales to any infrastructure composition — multi-region deployments using collections (`forEach`), conditional resources (`includeWhen`), custom readiness gates (`readyWhen`), external resource references, and composition of compositions (RGD chaining).

## Scope

### In Scope

- Declarative definition of custom Kubernetes APIs via ResourceGraphDefinitions
- CEL-based data flow with compile-time type checking against OpenAPI schemas
- Automatic dependency graph inference from CEL references
- CRD generation and dynamic controller registration
- Continuous server-side reconciliation of instances
- Conditional resource creation, dynamic collections, custom readiness
- External resource references (read-only access to existing resources)
- Composition of compositions (RGD chaining)
- Works with any Kubernetes resource type (native or CRD)

### Not In Scope

- Provider layer implementations; kro operates on resources, not on external dependencies
- A provider abstraction layer; kro works with any resources in a cluster, and does not require plugins or providers
- CI/CD pipeline integration; kro is the runtime, delivery tools bring manifests to the cluster

## Discussion and Notes

### Relationship to Existing Ecosystem

kro is complementary to, not competitive with, other Kubernetes tools.

- **Helm/Kustomize** handle packaging and delivery; kro handles runtime orchestration. Use GitOps to deliver RGDs and instances to clusters.
- **ACK/KCC/ASO/Crossplane providers** map cloud APIs to CRDs; kro composes those CRDs into higher-level abstractions. kro works with any operator's CRDs.
- **GitOps tools (Argo CD, Flux)** synchronize cluster state with git; kro processes user inputs server-side to create the actual resources. They handle delivery, kro handles composition.

### Path to v1

The project is actively iterating toward a v1 GA API. Features are proposed and tracked through individual KREPs, each reviewed and merged on its own timeline.
