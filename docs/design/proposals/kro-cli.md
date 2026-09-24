# KREP-021: kro CLI

> **Draft.** Supersedes [#1156](https://github.com/kubernetes-sigs/kro/pull/1156).
> Scope narrowed 2026-09 following review on [#1234](https://github.com/kubernetes-sigs/kro/pull/1234):

## Summary

This proposal makes the CLI a released artifact and gives it two jobs: offline authoring of
kro compositions, and installing and upgrading kro itself. Deterministic, scriptable, CI-friendly.

## Problem Statement

The current CLI is not very accessible today as it must be built from source. It should be readily available, and versioned along with the project.

It is also fairly limited in scope, in contrast to broad use cases for kro and what users may need a CLI for.

**Dry run**
Updating an RGD affects all instances immediately with no preview of impact. Teams need to see what will change, both to the RGD itself and to the resources it manages, before committing.
Because of [Deferred Schema Resolution](./deferred-schema-resolution.md), this has now limited capabilities.

**Bootstrapping Kro**
Helm charts offer limited capabilities in terms of upgrading CRDs and distribution. A CLI which has `kro bootstrap` alleviates
this problem and gives full control over how Kro gets into a cluster and what happens to it once it's there.

## Proposal

### Overview

Expand the existing `kro` CLI in `cmd/kro/`. Published as part of kro releases via GoReleaser.

### Command Summary

| Command                 | Description                                    | Status                       |
|-------------------------|------------------------------------------------|------------------------------|
| `kro validate rgd`      | Validate an RGD file                           | Existing, offline by default |
| `kro validate instance` | Validate an RGD and instance together          | New                          |
| `kro fmt`               | Normalize RGD YAML formatting                  | New                          |
| `kro bootstrap`         | Install and upgrade kro in an existing cluster | New                          |
| `kro check`             | Preflight and report an existing install       | New                          |
| `kro generate crd`      | Generate CRD from an RGD                       | Existing                     |
| `kro generate instance` | Generate sample instance from an RGD           | Existing                     |
| `kro generate diagram`  | Generate HTML dependency graph                 | Existing                     |

### Global Flags

Cluster-touching commands should have the standard kubeconfig subset: `--kubeconfig`, `--context`,
`--namespace`.

Commands that display data should have a standard display format `-o json|yaml`. Exit codes: `0` success, `1` error, `2` findings
without error (`fmt --check`).

### Design Details

#### Validate

`validate rgd` is offline by default. Schemas resolves from local CRD files or bundled
Kubernetes schemas. Cluster discovery is opt-in.

```bash
kro validate rgd -f rgd.yaml \
  --crds ./crds/              \  # directory of CRD files
  --kubernetes-version 1.30   \  # bundled schema version (default: latest)
  --from-cluster                 # discover CRDs and version from the API server
```

`validate instance` checks an instance against the schema the RGD would generate.

```bash
kro validate instance --rgd-file rgd.yaml --instance-file instance.yaml
```

`graph.NewBuilder` currently takes `*rest.Config` and `*http.Client` positionally
(`pkg/graph/builder.go:57`). Offline resolution requires an abstraction against these two.
That refactor results in code `generate` can then use.

Open: Should  `Graph` (KREP-024) is a first-class input alongside RGD. See
[Relationship to Existing KREPs](#relationship-to-existing-kreps).

#### Fmt

Normalize RGD YAML: field ordering, indentation, spacing. Pure text transformation.

```bash
kro fmt -f rgd.yaml         # in place
kro fmt -f rgd.yaml --check # exit 2 if changes needed
```

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

1. Does bootstrap own any dependency the way `clusterctl` owns cert-manager? kro appears
   to have none today. Confirm before designing around it.
2. Should upgrade run a kro-specific precheck, validating existing RGDs against the
   incoming controller's schema before swapping the Deployment?

Uninstall is out of scope, see [Not In Scope](#not-in-scope).

#### Cluster Dependency Summary

| Command             | Cluster Required                        |
|---------------------|-----------------------------------------|
| `validate rgd`      | No (default), Yes with `--from-cluster` |
| `validate instance` | No                                      |
| `fmt`               | No                                      |
| `generate *`        | Yes (existing behavior)                 |
| `bootstrap`         | Yes (No with `--export`)                |
| `check`             | Yes                                     |

## Other solutions considered

**Leave validation to the controller.** This is what is happening currently. Validation is slow, requires
apply, a cluster and a connection. It's cumbersome just to be informed that a CEL function has a typo in it.

**Helm for bootstrap.** Rejected under [Bootstrap](#bootstrap).

**Document `helm install`.** This is the original problem where CRDs will never be properly updated.

**Use the Helm SDK for bootstrapping.** This is explained in [Bootstrapping](#bootstrap) section.

## Relationship to Existing KREPs

| KREP                       | Relationship                                                                                               |
|----------------------------|------------------------------------------------------------------------------------------------------------|
| KREP-024 (Graph)           | RGD has a new runtime engine. **Open decision:** does the CLI accept `Graph` as a first-class input?      |
| KREP-013 (Graph Revisions) | Persisted revision history is the natural input for a future `diff` potentially                            |
| Deferred Schema Resolution | Validation offline changes since some objects will be resolved later. Still can validate CEL and others.   |
| KREP-002 (Collections)     | `forEach` expansion needs live collection contents, and dynamic GVKs compute the kind at evaluation. Offline `validate` compiles these nodes, it does not expand them. |

## Backward Compatibility

**Bootstrap installs different RBAC than `helm install` with chart defaults.** `values.yaml`
ships `rbac.mode: unrestricted` (`*/*` on `*`, retained for backwards compatibility) while
every rendered variant uses `aggregation`. Under aggregation the controller starts with no
permission over user resources until an aggregated ClusterRole labelled
`rbac.kro.run/aggregate-to-controller: "true"` is added.

`validate rgd` flips from cluster-required to offline by default. An RGD referencing CRDs that
exist only in-cluster now needs `--crds` or `--from-cluster`. The error must say so explicitly.

`generate *` is unchanged in v1. No API or controller changes.

## Scope

### In Scope

- Publishing the CLI as part of releases via GoReleaser
- Offline validation with local CRD files and bundled Kubernetes schemas
- Instance validation against RGD schemas
- `fmt`
- Bootstrapping and upgrading the kro controller and CRDs in an existing cluster

### Not In Scope

- **Package managers.** Homebrew, apt and friends are future work. Initial distribution is GitHub.
- **Helm-to-RGD conversion** and similar experimental workflows.
- **Telemetry.** No usage tracking. Feedback via GitHub issues and download metrics.
- **Uninstall.** The CRD cascade warrants its own proposal.

## Future Work

Deferred from the original draft [#1234](https://github.com/kubernetes-sigs/kro/pull/1234).

| Deferred                                | Why                                                                                                                                                                                                                                  |
|-----------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `kro lint`                              | No rule corpus yet. Revisit once real recurring problems are identified rather than guessed.                                                                                                                                         |
| `kro diff`                              | No use case yet. If it returns, comparing `GraphRevision` objects is the likely form, not two files on disk.                                                                                                                         |
| `kro preview`                           | Soft dependencies, collection caps, dynamic GVKs, impersonation, and pending propagation control (KREP-006). A client-side renderer cannot stay correct against a moving engine. Revisit when the engine can serve a dry run itself. |
| OCI push/pull/install, `registry login` | Needs scoping first: what is the artifact? RGD, Graph, bundle, or RGD plus default instance. Graph is arguably the better unit.                                                                                                      |

## Testing Strategy

### Requirements

Offline commands use fixtures. `bootstrap` uses envtest or kind.

### Test Plan

Unit tests per command. Integration coverage for the authoring loop (`validate` then `fmt`)
and the cluster loop (`check`, `bootstrap`, re-run as upgrade, prune, `--export` round trip).
Bootstrap is a bit more complex with upgrades, but not impossible to test with a kind cluster
and an integration suite.

## Discussion and notes

Reviews on [#1234](https://github.com/kubernetes-sigs/kro/pull/1234) suggested to tighten the scope.
This revision applies those reviews. `--namespace` and the kubeconfig flags were also raised there and are
now specified under [Global Flags](#global-flags).

Taken over from @jlbutler and @NicholasBlaskey 2026-09. Bootstrap added in rather than split
into its own KREP.

## Appendix: implementation plan

### Work items

1. **Release plumbing.** `.goreleaser.yaml`, CLI into the release pipeline. No design risk,
   unblocks distribution of everything else.
2. **Schema resolver abstraction.** Decouple `graph.NewBuilder` from `*rest.Config`. Bundle
   Kubernetes OpenAPI schemas and decide which versions to include.
3. **`validate instance`** on top of (2).
4. **`fmt`.**
5. **`bootstrap`.** `go:embed` over `manifests/rendered/*.yaml`, SSA with ordering, ApplySet
   prune via `pkg/controller/instance/applyset`, version detection. Plus `check`.

### Sequencing

(1) and (5) are independent of (2) and go first. (3) and (4) next (2).

### Risks

- The `graph.NewBuilder` refactor will touch the controller's compile path, not just the CLI.
- Deferred Schema Resolution may land first and change the resolver contract underneath (2).
- The Graph decision may change the final version of `validate` and `fmt`.
