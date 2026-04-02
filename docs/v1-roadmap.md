# kro v1 Roadmap

**Last Updated:** 2026-04-03
**Live Roadmap:** [kro.run/roadmap](https://kro.run/roadmap)

---

This is a cadence document tracking project release history, planned features, and implementation. It is a living document and will be updated frequently as a reference document describing project state. It is not intended to be a timeline for planning purposes.

## Release History of Major Features

### v0.4 — Jul 2025

| Feature | KREP | PR |
|---------|------|----|
| Status Conditions | [KREP-001](https://github.com/kubernetes-sigs/kro/pull/588) | [#544](https://github.com/kubernetes-sigs/kro/pull/544), [#588](https://github.com/kubernetes-sigs/kro/pull/588) |

### v0.8 — Jan 2026

| Feature | KREP | PR |
|---------|------|----|
| Collections (forEach) | [KREP-002](https://github.com/kubernetes-sigs/kro/pull/679) | [#936](https://github.com/kubernetes-sigs/kro/pull/936) |

### v0.9 — Mar 24, 2026

| Feature | KREP | PR |
|---------|------|----|
| Graph Revisions | [KREP-013](https://github.com/kubernetes-sigs/kro/pull/1174) | [#1174](https://github.com/kubernetes-sigs/kro/pull/1174) |
| Cluster-Scoped Instance CRDs | [KREP-010](https://github.com/kubernetes-sigs/kro/pull/1030) | [#1030](https://github.com/kubernetes-sigs/kro/pull/1030) |
| Decorators (design approved) | [KREP-003](https://github.com/kubernetes-sigs/kro/pull/738) | — |
| Template Field Omission (omit()) | [KREP-017](https://github.com/kubernetes-sigs/kro/pull/1121) | [#1121](https://github.com/kubernetes-sigs/kro/pull/1121) |
| includeWhen Resource References | [KREP-008](https://github.com/kubernetes-sigs/kro/pull/933) | [#933](https://github.com/kubernetes-sigs/kro/pull/933) |
| External Collections, External Resource Watches, 9 new CEL capabilities | — | See [release notes](https://github.com/kubernetes-sigs/kro/releases/tag/v0.9.0) |

Additionally shipped across v0.1–v0.9: Static Type Checking, External References, Breaking Schema Change Detection.

---

## Upcoming release snapshot

### v0.10 — ETA Mid-May 2026

| Feature | KREP | Implementation | Status |
|---------|------|---------------|--------|
| Variables | [KREP-011](https://github.com/kubernetes-sigs/kro/pull/1035) | [#1225](https://github.com/kubernetes-sigs/kro/pull/1225) | In progress |
| Label Migration (Phase 1) | [KREP-015](https://github.com/kubernetes-sigs/kro/pull/1094) | [#1095](https://github.com/kubernetes-sigs/kro/pull/1095) | In progress |
| Observability (events & metrics) | — | [#1208](https://github.com/kubernetes-sigs/kro/pull/1208), [#1114](https://github.com/kubernetes-sigs/kro/pull/1114) | In progress |

PRs potentially in scope for v0.10:
- Compile constant CEL expressions at eval time ([#1166](https://github.com/kubernetes-sigs/kro/pull/1166))
- CRD informer-backed schema resolver ([#1176](https://github.com/kubernetes-sigs/kro/pull/1176))
- SimpleSchema printColumn markers ([#1107](https://github.com/kubernetes-sigs/kro/pull/1107))
- Block RGD deletion while instances exist ([#1218](https://github.com/kubernetes-sigs/kro/pull/1218))
- Applyset orphan pruning in reverse topo order ([#1210](https://github.com/kubernetes-sigs/kro/pull/1210))

### Feature definition snapshot

| Feature | KREP | Implementation | Status |
|---------|------|----------------|--------|
| Decorators | [KREP-003](https://github.com/kubernetes-sigs/kro/pull/738) | — | Design approved |
| OwnerReferences & Deletion Policy | [KREP-004](https://github.com/kubernetes-sigs/kro/pull/763) | — | Draft. RGD/CRD lifecycle (distinct from KREP-014 which covers instance resources). |
| Level-Based Topological Sorting | [KREP-005](https://github.com/kubernetes-sigs/kro/pull/859) | — | KREP draft |
| Propagation Control | [KREP-006](https://github.com/kubernetes-sigs/kro/pull/861) | — | KREP in review |
| Horizontal Scaling (Scale subresource) | [KREP-007](https://github.com/kubernetes-sigs/kro/pull/866) | — | Open |
| Versioning Support | [KREP-009](https://github.com/kubernetes-sigs/kro/pull/935) | — | Open. Follow-on from KREP-013 (graph revisions). |
| Multi-Cluster (Cluster Targets) | [KREP-012](https://github.com/kubernetes-sigs/kro/pull/1064) | [#1223](https://github.com/kubernetes-sigs/kro/pull/1223) | KREP in review, impl started |
| Resource Lifecycles | [KREP-014](https://github.com/kubernetes-sigs/kro/pull/1091) | — | KREP in review |
| Resource Contribution | [KREP-016](https://github.com/kubernetes-sigs/kro/pull/1101) | — | Draft |
| Partial Dependencies in Branching | [KREP-018](https://github.com/kubernetes-sigs/kro/pull/1125) | — | KREP in review |
| Deferred Fields & Soft Dependencies | [KREP-019](https://github.com/kubernetes-sigs/kro/pull/1126) | — | Open |
| Dynamic Template Keys | [KREP-020](https://github.com/kubernetes-sigs/kro/pull/1127) | — | Draft |
| kro CLI | [KREP-021](https://github.com/kubernetes-sigs/kro/pull/1234) | — | KREP in review |
| managedResources in Instance Status | [KREP-022](https://github.com/kubernetes-sigs/kro/pull/1161) | — | KREP draft |
| Level-Aware Wavefront Controller | [KREP-023](https://github.com/kubernetes-sigs/kro/pull/1215) | — | Draft. Evolution of KREP-005. |
| Fine-Grained RBAC | — | — | Needs design |
| Schema Versioning & Automatic Upgrades | — | — | Needs design |

---

## Feature Descriptions

### Decorators (KREP-003)

Watch external resources and create companion resources. Enables platform-wide policy enforcement (VPAs, NetworkPolicies, PodDisruptionBudgets) without modifying existing RGDs. Design approved in v0.9; needs implementation owner.

### OwnerReferences & Deletion Policy (KREP-004)

RGD-to-CRD ownership and lifecycle management. Adds OwnerReferences between RGDs and their generated CRDs so that CRD cleanup follows standard Kubernetes garbage collection. Addresses deletion protection and the `allowCRDDeletion` controller flag. Distinct from KREP-014, which covers managed resource lifecycles at the instance level.

### Level-Based Topological Sorting (KREP-005)

Groups resources into dependency levels and executes resources within each level concurrently. Reconciliation time becomes proportional to dependency graph depth rather than total resource count. Significant performance improvement for large RGDs.

### Propagation Control (KREP-006)

Gates when mutations propagate to instances. `propagateWhen` CEL expressions control when mutations *start*; `readyWhen` controls when they *end*. Enables progressive rollouts, canary deployments, time-based windows, and manual approval gates. Combined with Graph Revisions, this enables controlled blast radius for production changes.

### Horizontal Scaling (KREP-007)

Adds scale subresource support to kro-generated CRDs, enabling HPA integration and `kubectl scale` for RGD instances.

### Versioning Support (KREP-009)

Broader versioning strategy beyond graph revisions (KREP-013). Addresses schema version evolution, multi-version CRD support, and conversion between API versions.

### Variables (KREP-011)

Graph nodes that compute values without creating resources. Enables breaking complex CEL logic into named, reusable steps rather than duplicating expressions across resource definitions. Variables participate fully in the DAG — they can depend on resources and other variables.

### Multi-Cluster (KREP-012)

Enables RGDs to deploy resources across multiple clusters via cluster targets. Addresses enterprise requirements for multi-region, disaster recovery, and geographic distribution. Implementation exploring multicluster-runtime integration.

### Resource Lifecycles (KREP-014)

Controls what happens to resources when instances are created or deleted. Includes resource adoption (claiming pre-existing resources into kro management) and deletion policies (`Delete | Orphan | Retain`) for safe production operations. Critical for migration from existing tools.

### Resource Contribution (KREP-016)

Mechanism for resources to contribute data back into the composition graph, enabling richer cross-resource data flow patterns.

### Partial Dependencies in Branching (KREP-018)

Allows CEL branching expressions (ternary, conditionals) to create partial dependencies — only the branch that evaluates true creates a dependency edge, rather than all referenced resources becoming hard dependencies.

### Deferred Fields & Soft Dependencies (KREP-019)

Enables fields to be populated after initial resource creation, decoupling creation ordering from data flow. Allows resources to be created earlier and patched later when upstream data becomes available.

### Dynamic Template Keys (KREP-020)

Allows CEL expressions in template map keys, not just values. Enables dynamic field names in resource templates.

### kro CLI (KREP-021)

CLI for authoring, validating, previewing, packaging and distributing RGDs. Includes offline validation, lint, fmt, diff, preview, and OCI distribution.

### managedResources in Instance Status (KREP-022)

Exposes the list of resources managed by an instance in its status, improving observability and enabling tooling to understand what an instance owns.

### Level-Aware Wavefront Controller (KREP-023)

Evolution of KREP-005. Redesigns the instance controller around level-aware wavefront processing for parallel resource reconciliation.

### Fine-Grained RBAC

Controls who can create RGDs and instances, prevents privilege escalation through RGD-created resources, and enables namespace-scoped multi-tenancy. Needs design.

### Schema Versioning

Support multiple schema versions with conversion rules, enabling API evolution without breaking existing instances. Intersects with Graph Revisions and instance pinning. Needs design.

---

## Related Documents

- [All KREPs](https://github.com/kubernetes-sigs/kro/pulls?q=label%3Akind%2Fkrep)
- [kro.run Roadmap](https://kro.run/roadmap)
