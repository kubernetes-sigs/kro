# Quality

Code is cheap to produce. Quality is cheap to improve. Every reconciliation pass pushes each
dimension closer to its asymptotic limit — bugs are squashed, performance improves, coverage
increases, duplicated code collapses. 100% on any dimension may be cost ineffective; the direction
matters more than arrival.

The default is no tradeoffs. When a conflict appears between quality dimensions, the design space
hasn't been explored enough. When the conflict is genuine and irreducible:

**Correctness > Performance > Observability > Testing > Simplicity**

## Correctness

The system converges to the state described by the designs from any starting point.

- [ ] The designs are implemented as stated
- [ ] No spurious errors on the happy path — errors are real errors
- [ ] Every error path is handled, propagated, and logged at the top
- [ ] System can crash at any line and recover without corrupting state
- [ ] Concurrent code is analyzed for race conditions

## Performance

Runtime cost is measured and budgeted. Resource consumption that isn't measured is invisible, and
invisible costs compound.

- [ ] Algorithmic complexity is optimal
- [ ] The system is profiled end-to-end; hot-path allocations and complexity are justified
- [ ] End-to-end benchmarks are committed and don't meaningfully regress
- [ ] Memory footprint is justified — only store and copy what's needed

## Observability

The system's runtime behavior is understandable from its outputs.

- [ ] Logs are accurate — ERROR after failures, INFO after side effects, logged once
- [ ] Logs are readable in plain English with structured context
- [ ] Errors compose into readable narratives
- [ ] Metrics exist for key operational signals

## Testing

Tests are how we know the system works and has not regressed. AI makes coverage cheap to produce.
Coverage should be as far to the edges as possible — integration tests survive refactors, unit tests
don't.

- [ ] Tests are a development bottleneck — optimize them
- [ ] Tests span the system and dependencies as practically as possible
- [ ] Correctness tests cover happy paths and edge cases
- [ ] Fault injection tests exercise error paths
- [ ] Regression tests accompany bug fixes and prevent recurrence

## Simplicity

The code's textual surface does not require invisible context to interpret correctly.

- [ ] Names are accurate and concise — no stuttering, no misleading verbs
- [ ] Code is well structured — clear abstractions, appropriate boundaries
- [ ] Validation is as far forward as possible — reject invalid state at the boundary
- [ ] No dead code or unreachable branches
- [ ] Duplicated code is collapsed — look for small conceptual tweaks that unify
- [ ] Types encode constraints — enums for closed sets, no unimplemented API fields
- [ ] Initialization is pulled to program start — no lazy setup buried in the call stack
