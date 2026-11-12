# KREP-021: kro CLI

> **Draft.** Supersedes [#1234](https://github.com/kubernetes-sigs/kro/pull/1234), which superseded [#1156](https://github.com/kubernetes-sigs/kro/pull/1156).
> Scope narrowed 2026-09 following review on [#1234](https://github.com/kubernetes-sigs/kro/pull/1234).

## Summary

This proposal makes the CLI a released artifact and gives it two jobs: offline authoring of
kro compositions, and installing and upgrading kro itself. Deterministic, scriptable, CI-friendly.

## Problem Statement

The current CLI is not very accessible today as it must be built from source. It should be readily available, and versioned along with the project.

It is also fairly limited in scope, in contrast to broad use cases for kro and what users may need a CLI for.

**Dry run**
Updating an RGD affects all instances immediately with no preview of impact. Teams need to see what will change, both to the RGD itself and to the resources it manages, before committing.
Because of [Deferred Schema Resolution](./deferred-schema-resolution.md), this has now limited capabilities.
This proposal does not solve it. `kro preview` and `kro diff` are deferred, see [Future Work](#future-work).

**Bootstrapping Kro**
Helm charts offer limited capabilities in terms of upgrading CRDs and distribution. A CLI which has `kro bootstrap` alleviates
this problem and gives full control over how Kro gets into a cluster and what happens to it once it's there.

## Proposal

### Overview

Expand the existing `kro` CLI in `cmd/kro/`. Published as part of kro releases via GoReleaser.

### Command Summary

| Command                 | Description                                    | Status                       |
|-------------------------|------------------------------------------------|------------------------------|
| `kro validate rgd`      | Validate an RGD file                           | Existing, becomes offline by default |
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

`--kubeconfig` and `--context` exist today but are ignored ([#1493](https://github.com/kubernetes-sigs/kro/issues/1493)):
every command builds its client with controller-runtime's `GetConfig()`. Cluster related commands should share one loader
instead of creating a new one.

Commands that display data should have a standard display format `-o json|yaml`. Exit codes: `0` success, `1` error, `2` findings
without error (`validate` on an invalid RGD or instance, `fmt --check` when changes are needed).

### Design Details

#### Validate

`validate rgd` is offline by default. Schemas resolve from local CRD files or bundled
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

Open: should `Graph` (KREP-024) be a first-class input alongside RGD? See
[Relationship to Existing KREPs](#relationship-to-existing-kreps).

#### Fmt

Normalize RGD YAML: field ordering, indentation, spacing. Pure text transformation.

- Comments are preserved.
- Top-level keys are ordered `apiVersion`, `kind`, `metadata`, `spec`. Keys under `spec` follow the
  field order of the `ResourceGraphDefinition` API types (`schema`, then `resources`).
  Keys inside resource templates keep the author's order, since `fmt` has no schema for them.
- Each document in a multi-document file is formatted in place. Document order is kept.

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

##### Decision: fetch the rendered manifests from the release

`kro bootstrap` downloads the rendered manifests from the GitHub release matching its own
version and applies them directly. Releases already publish them as assets
(`manifests/rendered/kro-*.yaml` in `.github/workflows/github-release.yaml`), so no new release
plumbing is needed. It does not link the Helm SDK to avoid go mod bloat and not be just a plain Helm wrapper.

Embedding the manifests with `go:embed` was rejected. `manifests/rendered/` is gitignored and
generated at release (`make render-static-manifests`, which needs helm and yq), and `go:embed`
fails when a pattern matches no files, so a clean checkout would not compile. `go:embed` also
cannot reach parent directories, so the embedding package would have to live under `manifests/`.

**Local manifests.** `--manifests <path>` skips the download and applies a local file instead.
Air-gapped clusters cannot reach the release and must point the image at a mirror, and dev
builds have no matching release. With `--manifests`, the installed version is read from the
`app.kubernetes.io/version` label in the file, not from the CLI version.

##### Module layout

`cmd/kro` is a separate Go module today and pins to kro `v0.9.3` (`cmd/kro/go.mod`). The CLI
therefore validates with a different engine than the controller released next to it.

**Proposal: fold `cmd/kro` into the root module.** The CLI builds from in-tree kro. The separate
module gives us little, since nobody imports the CLI's packages anyways. A `replace` directive is
not going to work, because `go install` rejects modules that have one. With nothing embedded,
`go install github.com/kubernetes-sigs/kro/cmd/kro@vX.Y.Z` works alongside the GoReleaser binaries.

Alternative: keep the module and automate the release: tag kro, bump `cmd/kro/go.mod` to that
tag, then build.

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
A kro CLI at vX.Y.Z installs kro vX.Y.Z. Upgrading kro means fetching a newer CLI. An older CLI
run against a newer install would be a downgrade, which bootstrap refuses without a force flag
(see [Version detection and downgrade](#upgrade)).

##### Upgrade

Re-running `kro bootstrap` against a cluster that already has kro is an upgrade. There is
no separate verb. kro is one component, so there is nothing for an upgrade planner to
schedule.

**Apply strategy.** Server-side apply with a stable field manager (`kro-bootstrap`).
Re-runs converge and correct drift caused by other tooling.
`--force-conflicts` would just overwrite.

**Ordering.** The `kro-system` Namespace and the ApplySet parent ConfigMap first, then CRDs,
wait for `Established`, then ServiceAccount and RBAC, then the Deployment last.

**CRD handling.** This is where a dedicated CLI will be more helpful.

- All crds under `helm/crds/` are managed by this bootstrapper. They are server-side
  applied on every run.
- All crds at the time of writing are `v1alpha1` so migration is not a concern yet.

**Pruning.** An upgrade path where something needs to be removed will, using normal apply,
orphan the resource.

Bootstrap reuses kro's existing ApplySet implementation in
`pkg/controller/instance/applyset`, which already implements KEP-3659 for the instance
controller: `Project`, `Apply`, `ListOrphans` and `DeleteOrphan`.
Incidentally, ownership is also taken care of by the ApplySet mechanism. Two changes are needed:

- **Parent.** `applyset.New` needs a parent object. Bootstrap uses a ConfigMap in `kro-system`.
  `Apply` returns the updated `Metadata`, and bootstrap writes it back to the ConfigMap before
  pruning, the same way the instance controller persists it on the instance.
- **Field manager.** The package hardcodes `kro.run/applyset` (`const.go`). It needs a
  configurable field manager so bootstrap applies as `kro-bootstrap`.

Two things that might be a problem still:

- **Prune only works if every apply succeeded.** A failed apply leaves that resource's UID out of
  `KeepUIDs`, so pruning would delete something still in use. Check `ApplyResult` errors
  first and abort the prune, not the whole run.
- **CRDs are applied but never pruned.** Deleting a CRD deletes every object of that kind,
  so pruning the RGD CRD would delete every RGD in the cluster. Bootstrap filters the
  `CustomResourceDefinition` GroupKind out of the `ListOrphans` result before calling `DeleteOrphan`.

Side effect worth having: the controller prunes instance resources through ApplySet and the
CLI prunes install components through ApplySet. One mechanism, one place to fix bugs.

**Version detection and downgrade.** The installed version is recorded as an annotation on the
ApplySet parent ConfigMap. Image tags are not reliable, since digest-pinned and locally built
images carry no version tag. Downgrades are only considered through a force flag.

##### Sketch

```bash
kro bootstrap                              # install or upgrade, current kubecontext
kro bootstrap --variant <name>             # named variant from manifests/variants.yaml
kro bootstrap --manifests ./kro.yaml       # local rendered manifests, no download
kro bootstrap --export                     # render to stdout, apply nothing
kro bootstrap --dry-run                    # server-side dry run, applies nothing
kro bootstrap --force-conflicts            # take ownership of conflicted fields
kro check                                  # preflight: API version, RBAC, existing install
```

##### Open questions

1. Does bootstrap own any dependency the way `clusterctl` owns cert-manager? kro appears
   to have none today. Confirm before designing around it.
2. Should upgrade run a kro-specific precheck, validating existing RGDs against the
   incoming controller's schema before swapping the Deployment?
3. Should `kro check` be its own command, or a `kro bootstrap --pre` flag like `flux check --pre`?

Uninstall is out of scope, see [Not In Scope](#not-in-scope).

#### Cluster Dependency Summary

| Command             | Cluster Required                        |
|---------------------|-----------------------------------------|
| `validate rgd`      | No (default), Yes with `--from-cluster` |
| `validate instance` | No                                      |
| `fmt`               | No                                      |
| `generate *`        | Yes (existing behavior)                 |
| `bootstrap`         | Yes (No with `--export`). Needs network access unless `--manifests` is set |
| `check`             | Yes                                     |

## Other solutions considered

**Leave validation to the controller.** This is what is happening currently. Validation is slow, requires
apply, a cluster and a connection. It's cumbersome just to be informed that a CEL function has a typo in it.

**Helm or the Helm SDK for bootstrap.** Rejected under [Bootstrap](#bootstrap).

**Document `helm install`.** This is the original problem where CRDs will never be properly updated.

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

**Existing Helm installs.** `kro bootstrap` refuses to run over a Helm-managed install
(`app.kubernetes.io/managed-by: Helm` on the controller Deployment). Taking over silently would
switch RBAC from `unrestricted` to `aggregation` and cut the controller's access to user resources.
The error explains the migration: `helm uninstall kro` (Helm keeps the CRDs in `crds/`), add the
aggregated ClusterRoles the RGDs need, then `kro bootstrap`.

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

1. **Release plumbing.** Fold `cmd/kro` into the root module, `.goreleaser.yaml`, CLI into the
   release pipeline. No design risk, unblocks distribution of everything else.
2. **Schema resolver abstraction.** Decouple `graph.NewBuilder` from `*rest.Config`. Bundle
   Kubernetes OpenAPI schemas and decide which versions to include.
3. **`validate instance`** on top of (2).
4. **`fmt`.**
5. **`bootstrap`.** Fetch the release's `kro-*.yaml` assets, SSA with ordering, ApplySet
   prune via `pkg/controller/instance/applyset` (ConfigMap parent, configurable field manager),
   version detection, Helm install detection. Plus `check`.

### Sequencing

(1) and (5) are independent of (2) and go first. (3) and (4) come after (2).

### Risks

- The `graph.NewBuilder` refactor will touch the controller's compile path, not just the CLI.
- Deferred Schema Resolution may land first and change the resolver contract underneath (2).
- The Graph decision may change the final version of `validate` and `fmt`.
