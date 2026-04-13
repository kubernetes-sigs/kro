---
name: quality
description: Project quality checklist — correctness, performance, observability, testing, simplicity.
---

# Quality

Designs are the desired state. Code is the current state. Every judgment below is relative to what
the designs say, not what the code currently does.

**Correctness > Performance > Observability > Testing > Simplicity**

Priority order for attention, findings, and time. The default is no tradeoffs; when the conflict is
genuine, this ordering decides.

Code is cheap to produce. Quality is cheap to improve. Don't cut scope — work through every item.

Structural before aesthetic — but aesthetics matter. Beautiful code tends to be correct; if it won't
read clean, the structure isn't right yet.

## Checklist

### Correctness

The system converges to the state described by the designs from any starting point.

- [ ] Designs are implemented as stated
- [ ] No spurious errors on the happy path — errors are real errors
- [ ] Every error path is handled, propagated, and logged at the top
- [ ] System can crash at any line and recover without corrupting state
- [ ] Concurrent code is analyzed for race conditions
- [ ] Think deeply about other correctness opportunities we might have missed

### Performance

Measure and budget runtime cost. Invisible resource consumption compounds.

- [ ] Algorithmic complexity is optimal
- [ ] System is profiled end-to-end; hot-path allocations and complexity are justified
- [ ] End-to-end benchmarks are committed and don't meaningfully regress
- [ ] Memory footprint is justified — only store and copy what's needed
- [ ] Think deeply about other performance opportunities we might have missed

### Observability

The system's runtime behavior is understandable from its outputs.

- [ ] Logs are accurate — ERROR after failures, INFO after side effects, logged once
- [ ] Logs are readable in plain English with structured context
- [ ] Errors compose into readable narratives
- [ ] Metrics exist for key operational signals
- [ ] Think deeply about other observability opportunities we might have missed

### Testing

Integration tests survive refactors, unit tests don't. Push coverage to the edges.

- [ ] Tests are a development bottleneck — optimize them
- [ ] Tests span the system and dependencies as practically as possible
- [ ] Correctness tests cover happy paths and edge cases
- [ ] Fault injection tests exercise error paths
- [ ] Regression tests accompany bug fixes — named Test<Feature>_Regression<Desc>, colocated with the feature they guard
- [ ] Tests do not flake — assertions observe completion, never guess timing
- [ ] Think deeply about other testing opportunities we might have missed

### Simplicity

Code's textual surface should not require invisible context to interpret correctly.

- [ ] Names are accurate and concise — no stuttering, no misleading verbs
- [ ] Each concept has exactly one name, used consistently across every surface
- [ ] Code is well structured — clear abstractions, appropriate boundaries
- [ ] Validation is as far forward as possible — reject invalid state at the boundary
- [ ] No dead code or unreachable branches
- [ ] Duplicated code is collapsed — look for small conceptual tweaks that unify
- [ ] Types encode constraints — enums for closed sets, no unimplemented API fields
- [ ] Initialization is pulled to program start — no lazy setup buried in the call stack
- [ ] Think deeply about other simplicity opportunities we might have missed
