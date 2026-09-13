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

**Bootstrapping Kro**
The CLI could be used to bootstrap Kro, ending the problem of where Kro is coming from and how it should be configured. Prior art: Flux, CAPI/CAPA and others. 

## Proposal

### Overview

Expand the existing `kro` CLI with new commands and improve existing ones. All changes are to the existing `cmd/kro/` codebase. The CLI is published as part of kro releases via GoReleaser.

### Command Summary

| Command                 | Description                                       | Status                            |
|-------------------------|---------------------------------------------------|-----------------------------------|
| `kro validate rgd`      | Validate an RGD file                              | Existing, updated for offline use |
| `kro validate instance` | Validate an RGD and instance together             | New                               |
| `kro lint`              | Check RGDs against conventions and best practices | New                               |
| `kro fmt`               | Normalize RGD YAML formatting                     | New                               |
| `kro diff`              | Structural diff between two RGD files             | New                               |
| `kro preview`           | Preview changes against live cluster state        | New                               |
| `kro push`              | Validate and push an RGD to an OCI registry       | New                               |
| `kro pull`              | Pull an RGD from an OCI registry                  | New                               |
| `kro install`           | Pull an RGD and apply it to the cluster           | New                               |
| `kro registry login`    | Authenticate to OCI registries                    | New                               |
| `kro generate crd`      | Generate CRD from an RGD                          | Existing                          |
| `kro generate instance` | Generate sample instance from an RGD              | Existing                          |
| `kro generate diagram`  | Generate HTML dependency graph                    | Existing                          |
| `kro bootstrap`         | Install and upgrade kro in an existing cluster    | New                               |

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

#### Bootstrap

`kro bootstrap` installs and upgrades the kro controller and CRDs in an existing cluster.

The plain helm install with the chart is missing a number of features that are required
in today's cluster management world: discovery, preflight checks, sensible opinionated
defaults and above all, a working, trustable *upgrade* flow.

##### Prior art

| Dimension       | Flux (`flux bootstrap` / `flux install`)                                 | CAPI (`clusterctl init`)                                                              |
|-----------------|--------------------------------------------------------------------------|---------------------------------------------------------------------------------------|
| Manifest source | Embedded in the binary (`go:embed`), rendered by `pkg/manifestgen`       | Fetched from provider release assets (`components.yaml` + `metadata.yaml`)            |
| Version model   | CLI version *is* the component version                                   | Resolves latest stable per provider; pin with `provider:vX.Y.Z`                       |
| Customization   | Kustomize overlay, `--export` to stdout                                  | Variable substitution from `clusterctl.yaml` or env                                   |
| Inventory       | Explicit object list in `.status.inventory`; labels link child to parent | A `Provider` object records version and inventory                                     |
| Re-run          | Idempotent, `--reconcile` updates in place                               | Errors if initialized; upgrade is a separate verb                                     |
| Upgrade         | Git push (self-managed) or re-run bootstrap                              | `upgrade plan` / `upgrade apply`, deletes components but preserves namespace and CRDs |
| Preflight       | `flux check --pre` (API version, RBAC)                                   | Detects cert-manager, installs it if absent, then owns it                             |
| GitOps handoff  | writes manifests to git, cluster syncs itself                            | None                                                                                  |

##### Decision: embed the rendered manifests

`kro bootstrap` embeds the release-rendered manifests with `go:embed` over
`manifests/rendered/*.yaml` and applies them directly. It does not link the Helm SDK to avoid
go mod bloat and not be just a plain Helm wrapper.

The Helm SDK was considered and rejected:

- **It adds nothing over `helm install`.** Helm is not in `go.mod`. Linking the SDK for template rendering plus
  `--set` passthrough would make `kro bootstrap` a reimplementation of
  `helm install kro oci://<repo>/kro --version vX.Y.Z`. The only advantage would be not
  needing the helm binary on PATH.
- **The cost is disproportionate.** The chart loader, registry client, ORAS, and a second
  set of Kubernetes client libraries, linked into a CLI whose other commands parse YAML
  and evaluate CEL. This would be an enormous intake of dependencies.
- **It reproduces the exact bug we are trying to fix.** Helm never upgrades or deletes
  anything in a chart's `crds/` directory.

##### Version

`--version` can only mean "the version this binary shipped with". This is the Flux model.
A kro CLI at vX.Y.Z installs kro vX.Y.Z. Upgrading kro means fetching a newer CLI, which
makes version skew between CLI and controller is impossible.

##### Upgrade

Re-running `kro bootstrap` against a cluster that already has kro is an upgrade. There is
no separate verb. kro is one component, so there is nothing for an upgrade planner to
schedule.

**Apply strategy.** Server-side apply with a stable field manager(`kro-bootstrap`).
Re-runs converge to detect drift cause by possible other tooling.
`--force-conflicts` would just overwrite.

**Ordering.** CRDs first, wait for `Established`, then ServiceAccount and RBAC, then the
Deployment last.

**CRD handling.** This is where a dedicated CLI will be more helpful.

- All crds under `helm/crds/` are managed by this bootstrapper. They are server-side
  applied on every run.
- All crds at the time of writing are `v1alpha1` so migration is not a concern yet.

**Pruning.** An upgrade path where something needs to be removed will, using normal apply,
orphan the resource.

Bootstrap reuses kro's existing ApplySet implementation in
`pkg/controller/instance/applyset`, which already implements KEP-3659 for the instance
controller. Nothing new to build: `Project`, `Apply`, `ListOrphans` and `DeleteOrphan`.
Incidentally, ownership is also taken care of by the ApplySet mechanism.

Two things that might be a problem still:

- **Prune only works if every apply succeeded.** A failed apply leaves that resource's UID out of
  `KeepUIDs`, so pruning would delete something still in use. Check `ApplyResult` errors
  first and abort the prune, not the whole run.
- **CRDs are applied but never pruned**, for the cascade reason under Uninstall.

Side effect worth having: the controller prunes instance resources through ApplySet and the
CLI prunes install components through ApplySet. One mechanism, one place to fix bugs.

**Version detection and downgrade.** Current version is read from the controller Deployment
image tag, downgrades are only considered through a force flag.

**Uninstall.** Removal deletes the components and *preserves the CRDs by default*.

##### Sketch

```bash
kro bootstrap                              # install or upgrade, current kubecontext
kro bootstrap --variant <name>             # named variant from manifests/variants.yaml
kro bootstrap --export                     # render to stdout, apply nothing
kro bootstrap --dry-run                    # server-side dry run, shows the diff
kro bootstrap --force-conflicts            # take ownership of conflicted fields
kro check                                  # preflight: API version, RBAC, existing install (should this be separate?)
```

##### Open questions

1. Is uninstall in scope here, or its own proposal? The CRD cascade argues for its own
   design, with confirmation prompts and a `--keep-crds` default.
2. Does bootstrap own any dependency the way `clusterctl` owns cert-manager? kro appears
   to have none today. Confirm before designing around it.
3. Naming collision: `kro install` versus `kro bootstrap`.
4. Should upgrade run a kro-specific precheck, validating existing RGDs against the
   incoming controller's schema before swapping the Deployment?

#### Cluster Dependency Summary

| Command                     | Cluster Required                        |
|-----------------------------|-----------------------------------------|
| `validate rgd`              | No (default), Yes with `--from-cluster` |
| `validate instance`         | No                                      |
| `lint`                      | No                                      |
| `fmt`                       | No                                      |
| `diff`                      | No                                      |
| `preview`                   | Yes                                     |
| `push` / `pull` / `install` | No / No / Yes (for apply)               |
| `generate *`                | Yes (existing behavior)                 |
| `bootstrap`                 | Yes (No with `--export`)                |

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
- Bootstrapping and upgrading the kro controller and CRDs in an existing cluster

### Not In Scope

- **Package managers.** Homebrew, apt, etc. are potential future work. Initial distribution mechanism is GitHub.
- **Experimental features.** Helm-to-RGD conversion and similar workflows may come later as the CLI matures.
- **Telemetry.** No usage tracking. Feedback via GitHub issues, download metrics, and community feedback.

## Testing Strategy

### Requirements

No additional infrastructure needed. Offline commands are tested with fixture files. Cluster-dependent commands are tested against envtest or kind.

### Test Plan

Unit tests for each command's core logic. Integration tests for the full CLI workflow (validate → lint → push → pull → install → preview). OCI tests against a local registry.