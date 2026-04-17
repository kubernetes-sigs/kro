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

Do not modify stable code (`pkg/`, `cmd/`, `api/`, `docs/`, `test/`)
unless upstreaming a change to `kubernetes-sigs/kro`.

Experimental work lives under `experimental/`. Read
`experimental/docs/design/` before implementing changes.

## Skills

Project-local skills live in `.skills/`. These are operational checklists
the agent loads and executes — distinct from designs, which declare
desired state.

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

## Testing

    cd experimental && make presubmit

`experimental/Makefile` defines common development recipes (`run`, `apply`,
`test`, `bench`). Check there before reaching for raw `go` commands.

If working on upstream compat (`experimental/test/graph-compat/`), read
the `##@ Upstream compat` section of `experimental/Makefile` — it
documents the per-file loop (`compat-one`, `compat-presubmit`,
`compat-count`) and the allowlist contract.
