# KREP-021: kro CLI

## Summary

The kro CLI exists today with basic validate and generate commands, all requiring a cluster connection and only available by building from source. This proposal expands the CLI into a comprehensive tool for authoring, validating, distributing, and previewing kro ResourceGraphDefinitions — offline where possible, with cluster access when needed. The CLI ships as part of kro releases.

The CLI is the primitive layer for working with kro compositions. It is deterministic, scriptable, suitable for CI, and agent-friendly.

## Problem Statement

The current CLI is not very accessible today as it must be built from source. It should be readily available, and versioned along with the project.

It is also fairly limited in scope, in contrast to broad use cases for kro and what users may need a CLI for.

**Dry run**
Updating an RGD affects all instances immediately with no preview of impact. Teams need to see what will change, both to the RGD itself and to the resources it manages, before committing.

**Distribution**
There is no standard way to package and distribute RGDs. Users resort to git repositories and kubectl apply. The Kubernetes ecosystem thrives on reusable artifacts (Helm charts, OCI images) — RGDs need the same distribution model.

**Validation**
The only way to know if an RGD is valid today is to apply it to a cluster. Errors surface at reconciliation time, not authoring time. Users need fast feedback in their local workflow and CI pipelines, without requiring a live cluster.

**Authoring tools**
The CLI does not currently offer diff, linter, or other authoring tools to help users adopt and succeed with kro.

## Proposal

### Overview

Expand the existing `kro` CLI with new commands and improve existing ones. All changes are to the existing `cmd/kro/` codebase. The CLI is published as part of kro releases via GoReleaser.

### Command Summary

| Command | Description | Status |
|---------|-------------|--------|
| `kro validate rgd` | Validate an RGD file | Existing, updated for offline use |
| `kro validate instance` | Validate an RGD and instance together | New |
| `kro lint` | Check RGDs against conventions and best practices | New |
| `kro fmt` | Normalize RGD YAML formatting | New |
| `kro diff` | Structural diff between two RGD files | New |
| `kro preview` | Preview changes against live cluster state | New |
| `kro push` | Validate and push an RGD to an OCI registry | New |
| `kro pull` | Pull an RGD from an OCI registry | New |
| `kro install` | Pull an RGD and apply it to the cluster | New |
| `kro registry login` | Authenticate to OCI registries | New |
| `kro generate crd` | Generate CRD from an RGD | Existing |
| `kro generate instance` | Generate sample instance from an RGD | Existing |
| `kro generate diagram` | Generate HTML dependency graph | Existing |

### Design Details

#### Validate

Update existing `validate rgd` to work offline by default. The current implementation requires a cluster connection for CRD schema resolution. The updated command resolves schemas from local CRD files or bundled Kubernetes schemas, with cluster discovery as an opt-in flag.

```bash
kro validate rgd -f rgd.yaml \
  --crds ./crds/              \  # Optional: directory of CRD files for schema resolution
  --kubernetes-version 1.30   \  # Optional: built-in schema version (default: latest bundled)
  --from-cluster                 # Optional: discover CRDs and version from API server
```

New `validate instance` command validates an RGD and a sample instance together — confirming the instance conforms to the schema the RGD would generate.

```bash
kro validate instance --rgd-file rgd.yaml --instance-file instance.yaml
```

#### Lint

Check RGDs against conventions and best practices beyond schema validity. Lint rules are deterministic checks against the RGD structure — no cluster connection required.

Examples: resource ID naming conventions, CEL expression style, required annotations, status field coverage, SimpleSchema usage patterns.

```bash
kro lint -f rgd.yaml
```

#### Fmt

Normalize RGD YAML. Consistent field ordering, indentation, and formatting for readability and consistency. Pure text transformation — no cluster, no schema knowledge.

```bash
kro fmt -f rgd.yaml        # in-place
kro fmt -f rgd.yaml --check # exit non-zero if changes needed (for CI)
```

#### Diff

Structural diff between two RGD files. Surfaces field additions, removals, type changes, and CEL expression changes. No cluster required.

```bash
kro diff -f old-rgd.yaml -f new-rgd.yaml
```

This is distinct from `preview`, which diffs against live cluster state.

#### Preview

Preview the impact of changes against a live cluster. Requires API server access. Performs client-side diffs to show what would be created, updated, or deleted.

For instances — shows the resource diff:
```bash
kro preview -f instance.yaml
```

For RGDs — shows the RGD diff and the impact on existing instances:
```bash
kro preview -f rgd.yaml
```

Example output:
```
UPDATE: ResourceGraphDefinition my-app (kro.run/v1alpha1)
  spec.schema.spec:
-   replicas: int
+   replicas: int | default=3

Affected instances (2): my-app-prod, my-app-dev

UPDATE: Deployment my-app-prod-deployment (apps/v1) [instance: my-app-prod]
  spec:
-   replicas: 1
+   replicas: 3
```

#### OCI Distribution

RGDs are packaged as OCI artifacts and published to repositories in OCI-compliant registries. Uses ORAS for registry interaction.

```bash
kro push -f rgd.yaml registry.io/org/my-rgd:v1.0.0
kro pull registry.io/org/my-rgd:v1.0.0 -o rgd.yaml
kro install registry.io/org/my-rgd:v1.0.0
```

`push` validates the RGD before pushing — invalid compositions cannot be published. `install` is a convenience that pulls and applies in one step.

#### Registry Authentication

The CLI looks for credentials in order:
1. Credentials set by `kro registry login` (stored at `~/.kro/config.json`)
2. Credentials set by `docker login` and friends

```bash
kro registry login registry.io -u username --password-stdin
```

Supports standard OCI auth options (TLS certs, CA bundles, insecure).

#### Cluster Dependency Summary

| Command | Cluster Required |
|---------|-----------------|
| `validate rgd` | No (default), Yes with `--from-cluster` |
| `validate instance` | No |
| `lint` | No |
| `fmt` | No |
| `diff` | No |
| `preview` | Yes |
| `push` / `pull` / `install` | No / No / Yes (for apply) |
| `generate *` | Yes (existing behavior) |

## Scope

### In Scope

- Publishing the CLI as part of releases via GoReleaser
- Offline validation with local CRD files and bundled Kubernetes schemas
- Instance validation against RGD schemas
- Lint and fmt commands
- Structural diff between RGD files
- Preview of changes against live cluster
- OCI packaging and distribution via ORAS
- Registry authentication

### Not In Scope

- **Package managers.** Homebrew, apt, etc. are potential future work. Initial distribution mechanism is GitHub.
- **Experimental features.** Helm-to-RGD conversion and similar workflows may come later as the CLI matures.
- **Telemetry.** No usage tracking. Feedback via GitHub issues, download metrics, and community feedback.

## Testing Strategy

### Requirements

No additional infrastructure needed. Offline commands are tested with fixture files. Cluster-dependent commands are tested against envtest or kind.

### Test Plan

Unit tests for each command's core logic. Integration tests for the full CLI workflow (validate → lint → push → pull → install → preview). OCI tests against a local registry.
