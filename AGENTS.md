# kro — Experimental Development (krocodile)

## Repository

Fork of `kubernetes-sigs/kro`. Remote `origin` is `ellistarn/kro`,
`upstream` is `kubernetes-sigs/kro`.

## PR Target

All PRs target `ellistarn/kro`, base branch `krocodile`.
Not `main`. Not upstream.

GitHub defaults PRs against upstream (`kubernetes-sigs/kro`). Always use
the explicit fork URL when creating PRs:

    gh pr create --repo ellistarn/kro --base krocodile

Or open in browser:

    https://github.com/ellistarn/kro/compare/krocodile...<branch>?expand=1

## Worktrees

All implementation happens in `.worktrees/`. The main checkout stays on
`krocodile`. Create topic branches off `krocodile` for PRs.

## Layout

### Stable code (upstream kro)

Standard Kubernetes controller project layout:

- `pkg/` — Core libraries (CEL engine, graph builder, controllers, runtime, etc.)
- `cmd/` — Binary entrypoints
- `api/` — CRD type definitions
- `docs/design/` — Accepted design proposals
- `examples/` — Stable example manifests
- `test/` — Integration, E2E, and upgrade tests

Do not modify stable code unless upstreaming a change to `kubernetes-sigs/kro`.

### Experimental code (`experimental/`)

Each experiment is named under `experimental/<name>/`. Designs live at
`experimental/docs/design/<name>/`.

- `experimental/<name>/controller/` — Go packages
- `experimental/<name>/examples/` — Example manifests
- `experimental/docs/design/<name>/` — Design documents

### Active experiments

- **`experimental/graph/`** — POC Graph controller. Runtime reconciliation
  of Graph CRs via DAG walk, CEL evaluation, and server-side apply.
  Imports from `pkg/cel`, `pkg/simpleschema`, `pkg/cel/conversion`.

## Dependency Invariant

`experimental/` may import from `pkg/`. `pkg/` must never import from
`experimental/`. This is structural — it makes graduation a move
operation, not surgery.

## Rebase Strategy

`krocodile` periodically rebases onto `upstream/main` to pick up kro
library changes. Conflicts stay scoped because dependency flows one way.

## Graduation

When experimental code earns its place in `pkg/`, move it. The import
path change is scoped to experimental consumers only.
