# KREP: Instance Propagation Control

## Summary

This proposal introduces `propagateWhen`, an RGD-level mechanism to control
**when** instances of a ResourceGraphDefinition are allowed to reconcile against
a new GraphRevision. When an RGD spec changes and a new revision is issued,
instead of all instances immediately reconciling, CEL expressions are evaluated
across the instance population to determine which instances may proceed. This
enables gradual rollout, canary validation, and blast radius control.

## Motivation

When an RGD spec changes today, kro issues a new GraphRevision and immediately
re-enqueues **all** instances via `DynamicController.Register()`. Every instance
unconditionally resolves the latest compiled graph and reconciles. There is no
mechanism to gate, batch, or order how instances adopt a new revision.

This is dangerous for platform teams managing many instances of a single RGD. A
bad change to an `Application` RGD that manages 200 instances immediately
affects all 200. There is no way to validate the change on a subset first, no
way to gate propagation on health signals, and no way to slow down the rate of
change.

### Concrete example

A platform team runs an RGD defining a `WebApp` custom resource. 200 instances
exist across the cluster. The team updates the RGD to change the deployment
strategy from RollingUpdate to Recreate.

Without propagation control: all 200 instances immediately reconcile. Every
WebApp's deployment is replaced simultaneously, causing cluster-wide downtime.

With propagation control: the team configures `propagateWhen` to require that
50% of already-propagated instances must be healthy before the next batch
proceeds. The first instance updates, proves healthy, then the next batch
follows. If the first instance fails its readiness checks, propagation halts
and the team is alerted.

### Relationship to KREP-006

[KREP-006](https://github.com/kubernetes-sigs/kro/pull/861) introduced the
concept of `propagateWhen` broadly — covering both intra-instance (collection
resources within a graph) and cross-instance (instances of an RGD) propagation.
This proposal focuses exclusively on the **cross-instance** case and defines the
concrete implementation primitives needed to achieve it.

### Relationship to KREP-013

[KREP-013](graph-revisions.md) introduced `GraphRevision` as an immutable
snapshot of a compiled RGD spec with monotonically increasing revision numbers.
This proposal builds directly on that foundation — `GraphRevision` provides the
stable revision identity that propagation control gates against.

## Proposal

### Overview

The core design introduces five primitives:

1. **Propagation Orchestrator** — a sub-step of the RGD controller that
   evaluates `propagateWhen` and selectively enqueues instances.
2. **RevisionReconciled Condition** — an instance-level condition tracking which
   GraphRevision an instance last successfully reconciled against.
3. **Propagated Condition** — an RGD-level condition reporting propagation
   progress.
4. **Propagation CEL Environment** — a cross-instance CEL context with full
   object access and helper methods.
5. **Dynamic Parent Informer** — full object cache for instance CRDs, enabling
   the orchestrator to read instance state without API calls.

### Design details

#### 1. All Instances Target Latest — No Revision Pinning

Instances always move towards the latest revision. The orchestrator controls
*when* an instance is re-enqueued, not *which* revision it targets. Once an
instance reconciles, it gets the latest.

The gate is: "should this instance be re-enqueued yet?" not "which revision
should it use?"

This simplifies the model: there is exactly one target state (latest revision),
and the orchestrator decides the pace at which instances converge to it.

#### 2. Propagation Orchestrator

The propagation orchestrator runs as a sub-step of the RGD controller
reconciliation, after `ensureServingState()` succeeds.

Rationale for this placement:
- The RGD controller already owns the "activate revision for serving" lifecycle.
- It has access to the compiled graph and revision number.
- A single point of coordination avoids consistency issues (multiple instances
  independently deciding they're in the same batch).
- It reads instance state from the parent informer cache (O(N) in-memory, no
  API calls).

The orchestrator's algorithm:

```
1. Read all instances from the parent informer cache (full objects)
2. For each instance, read the RevisionReconciled condition
3. Determine which instances are at the latest revision vs pending
4. For each pending instance, evaluate propagateWhen CEL expressions
5. Instances where all expressions return true → enqueue for reconciliation
6. Update the RGD's Propagated condition with progress
7. If propagation is still in-flight → return RequeueAfter
```

#### 3. Instance Propagation Gating

When `propagateWhen` is configured, instances must not blindly reconcile against
the latest revision. Two scenarios need distinct handling:

**Scenario A — RGD spec changes (new GraphRevision issued):**

The orchestrator decides which instances advance. Only cleared instances are
enqueued. Un-cleared instances are not enqueued and do not reconcile against
the new revision.

**Scenario B — Individual instance spec changes (user updates their instance):**

A user may update their instance directly (e.g., changing a field in their
Application CR). In this case, the instance should reconcile using its
**current** GraphRevision — not the latest — unless the orchestrator has already
cleared it to propagate.

Resolution: The propagation registry (in-memory, maintained by the orchestrator)
tracks which instances are cleared to propagate. The instance controller
consults this registry in `resolveCompiledGraph()`:

- If the instance is marked "cleared" in the registry → use latest revision
- If the instance is NOT cleared → use the revision from its
  RevisionReconciled condition (stay on current revision while reconciling the
  user's spec change)

This keeps the instance controller stateless with respect to propagation — it
delegates the decision to the registry.

#### 4. RevisionReconciled Condition

A new condition on instance status:

```yaml
status:
  conditions:
    - type: "RevisionReconciled"
      status: "True"
      reason: "7"                          # revision number
      lastTransitionTime: "2026-04-30T12:00:00Z"
```

Semantics:
- **type**: `RevisionReconciled`
- **status**: `True` when the instance has successfully reconciled at the noted
  revision (state reaches Active)
- **reason**: The revision number the instance reconciled against
- **lastTransitionTime**: When this revision was first successfully reconciled

The instance controller sets this condition at the end of a successful
reconciliation when the instance reaches Active state.

Design considerations:
- `lastTransitionTime` enables future time-based propagation rate control (e.g.,
  "propagate 5 instances every 1 hour" by checking transition timestamps).
- `Ready` condition remains for user-facing readiness; `RevisionReconciled` is
  kro-internal machinery.
- The reason field as revision number allows the orchestrator to determine
  revision currency by comparing against the latest issued revision.

#### 5. Propagated Condition on RGD

A new condition on the RGD status reporting propagation progress:

```yaml
status:
  conditions:
    - type: "Propagated"
      status: "True"         # All instances have propagated
      reason: "AllInstancesUpdated"
      message: "15/15 instances reconciled at revision 7"
```

States:
- `True` — all instances have reconciled at the latest revision
- `False` — propagation is in-flight (some instances pending)
- `Unknown` — error evaluating propagateWhen expressions

When `propagateWhen` is not configured, this condition is not set (preserving
existing behavior).

#### 6. Dynamic Parent Informer

Currently the `WatchManager` uses metadata-only informers
(`k8s.io/client-go/metadata`). For the parent GVR (instance CRD) only, we
switch to a full dynamic informer (`k8s.io/client-go/dynamic`).

This gives us complete instance objects in cache — spec, status, conditions —
enabling:
- Reading `RevisionReconciled` condition directly from cache
- Passing full instance data into the propagateWhen CEL environment
- Richer expressions referencing instance spec/status fields

Scope: Only the parent informer switches to dynamic. Child/external resource
watches remain metadata-only.

Trade-offs:
- Higher memory footprint (full objects vs metadata-only)
- Required change to the parent watch path in DynamicController
- Necessary for the CEL environment to have full instance visibility

#### 7. Propagation CEL Environment

`propagateWhen` expressions need a CEL context fundamentally different from
per-instance graph expressions. They need cross-instance visibility.

**Variables:**

| Variable | Type | Description |
|----------|------|-------------|
| `<singular>` (e.g., `application`) | object | The current instance being evaluated |
| `<plural>` (e.g., `applications`) | list | All instances of this RGD |

The singular/plural names are derived from the CRD generated by kro's schema
(available in the compiled graph's `Instance.Meta`).

**Helper methods:**

| Method | Returns | Description |
|--------|---------|-------------|
| `.ready()` | bool | True if the instance's `Ready` condition is True |
| `.updated()` | bool | True if RevisionReconciled reason matches latest revision |

**Full object access:**

All standard Kubernetes object fields are accessible:
- `.metadata.name`, `.metadata.namespace`, `.metadata.creationTimestamp`
- `.spec.*` — full spec access
- `.status.*` — full status access including conditions

**Example expressions:**

```yaml
# Canary: first instance always proceeds, others wait until at least one is
# updated and ready. This ensures a single canary validates the new revision
# before the rest follow.
propagateWhen:
  - "${applications.filter(a, a.updated() && a.ready()).size() > 0 || application == applications[0]}"
```

```yaml
# Error halt: block all propagation if any instance is in an error state.
# This is a global gate — it evaluates to the same bool for every instance.
# When true, all pending instances are cleared simultaneously.
propagateWhen:
  - "${!applications.exists(a, a.status.state == 'ERROR')}"
```

```yaml
# Spec-aware tiered promotion: staging instances proceed first, production
# instances only after all staging instances have updated and are ready.
# References `application` (singular), so different instances get different
# results.
propagateWhen:
  - "${application.spec.tier != 'production' || applications.filter(a, a.spec.tier == 'staging' && (!a.updated() || !a.ready())).size() == 0}"
```

Expression semantics: all expressions in the list must evaluate to true (logical
AND) for the instance to proceed. Expressions that reference `application`
(singular) discriminate between instances; expressions that only reference
`applications` (plural) act as global gates.

See [propagationExamples.md](propagationExamples.md) for additional examples.

#### 8. Selective Re-enqueue

Today `Register()` re-enqueues ALL instances unconditionally. With propagation
control, only instances cleared by the orchestrator should reconcile.

The orchestrator directly enqueues cleared instances into the
DynamicController's work queue. This requires exposing an
`Enqueue(gvr, namespacedName)` method on DynamicController.

When `propagateWhen` is configured:
- `Register()` is called without the blanket re-enqueue (new variant or flag)
- The orchestrator evaluates propagateWhen and enqueues the first batch
- As instances complete, re-evaluation (section 9) wakes the orchestrator
- The orchestrator enqueues the next batch

When `propagateWhen` is NOT configured:
- Behavior is unchanged — `Register()` re-enqueues all instances as today

#### 9. Re-evaluation Trigger

When an instance finishes reconciling at the new revision, the orchestrator must
re-evaluate to potentially advance the next batch.

For the initial implementation: **periodic poll**. When propagation is in-flight,
the RGD controller returns `RequeueAfter` with a configurable interval (default
30s).

Alternatives considered for future optimization:

| Option | Mechanism | Pro | Con |
|--------|-----------|-----|-----|
| A | Parent informer Update event triggers RGD re-reconcile | Low latency, reuses existing infra | Noisy — every instance status update triggers RGD reconcile |
| B | Instance controller directly signals orchestrator | Lowest latency, precise | Tight coupling, shared state |
| D | Periodic poll (recommended) | Simple, correct, no coupling | Latency bounded by interval |

Start with Option D. Optimize to event-driven later with data on latency needs.

#### 10. Feature Gate

All new code paths are gated behind a `PropagationControl` feature flag (Alpha,
default disabled). When disabled, `propagateWhen` field is ignored and behavior
is unchanged.

### API Changes

```go
type ResourceGraphDefinitionSpec struct {
    Schema        *Schema     `json:"schema,omitempty"`
    Resources     []*Resource `json:"resources,omitempty"`
    // PropagateWhen defines CEL expressions that gate instance updates when a
    // new GraphRevision is issued. All expressions must evaluate to true for an
    // instance to proceed. Empty list means no gate (all instances update
    // immediately, preserving current behavior).
    PropagateWhen []string    `json:"propagateWhen,omitempty"`
}
```

New condition types:
- `RevisionReconciled` on instances
- `Propagated` on RGDs

No new CRDs are introduced.

## Other solutions considered

**Revision pinning (target-revision label on instances):** Instances would be
pinned to specific revisions via a label, with the orchestrator advancing the
label when ready. Rejected because it adds complexity — instances must reason
about which revision to use. The simpler model (always target latest, control
enqueue timing) achieves the same outcome with less machinery.

**Instance self-evaluation (each instance lists siblings):** Each instance
would list all sibling instances from the informer cache and independently
evaluate propagateWhen. Rejected because it creates consistency issues — multiple
instances may simultaneously conclude they're in the same batch — and it moves
coordination logic into N controllers instead of one.

**Separate PropagationControl CRD:** A PDB-like CRD decoupled from the RGD.
Rejected because it introduces mapping challenges between RGD and the control
CRD, and KRO would need to handle eventual consistency between them. An inline
field on the RGD is atomic with the graph definition.

**Separate propagation controller:** A new controller dedicated to orchestrating
propagation. Rejected because the RGD controller already owns the revision
lifecycle and has all necessary context. A new controller adds an informer, a
queue, and a lifecycle without clear benefit.

## Scoping

#### What is in scope for this proposal?

- `propagateWhen` field on `ResourceGraphDefinitionSpec`
- Propagation orchestrator logic in the RGD controller
- `RevisionReconciled` condition on instances
- `Propagated` condition on RGDs
- Propagation CEL environment with `.ready()` and `.updated()` helpers
- Propagation registry (in-memory) for instance-level gating
- Selective enqueue via `DynamicController.Enqueue()`
- Dynamic parent informer (full object cache for instance CRDs)
- `PropagationControl` feature gate
- Periodic re-evaluation trigger

#### What is not in scope?

- **Instance ordering / explicit batch assignment** — users write ordering logic
  in CEL expressions directly for now.
- **Built-in functions** (`exponentiallyReady`, `linearlyReady`) — deferred to a
  follow-up. Users write equivalent CEL for now.
- **Manual approval gates** — requires external input mechanism.
- **Time-based windows** — requires `time.now()` in CEL, which has determinism
  concerns.
- **Automatic rollback on failure** — propagation halts, but does not revert.
- **Per-instance propagateWhen override** — all instances share the same gate.
- **Overlapping propagations** — if the RGD changes again mid-propagation, the
  new revision takes over. No concurrent propagation tracking.
- **Interaction with `includeWhen`** — deferred to future analysis.
- **Intra-instance propagation** (`propagateWhen` on individual resources within
  a graph) — covered by KREP-006's broader scope, not this proposal.
- **Event-driven re-evaluation** (Option A/B) — start with polling, optimize
  later.

## Testing strategy

#### Requirements

- envtest (simulated API server) for integration tests
- Multiple instances of a generated CRD to validate cross-instance behavior
- Ability to simulate RGD spec changes and observe propagation ordering

#### Test plan

**Unit tests:**
- Propagation CEL environment: variable binding, `.ready()`, `.updated()`
  helper evaluation
- Orchestrator logic: batch selection given various instance states
- Registry: cleared/not-cleared state management, rebuild from conditions

**Integration tests:**
- Create RGD with `propagateWhen`, create N instances, update RGD spec, verify
  only instances where expressions evaluate to true are reconciled
- Verify instances not yet cleared continue using their current revision when
  their spec changes
- Verify `Propagated` condition transitions: False during rollout, True when
  complete, Unknown on CEL error
- Verify `RevisionReconciled` condition is set with correct revision number
  and timestamps

**Backward compatibility tests:**
- RGD without `propagateWhen` behaves identically to today (all instances
  re-enqueue immediately)
- Feature gate disabled: `propagateWhen` field is ignored

**Edge case tests:**
- Zero instances exist when propagation starts
- Instance deleted mid-propagation
- RGD spec changes again while propagation is in-flight (new revision supersedes)
- All propagateWhen expressions fail (CEL errors → Propagated=Unknown)
- Instance never reaches Active (propagation halts, doesn't proceed indefinitely)

## Open questions

1. **Poll interval:** What should the default RequeueAfter interval be when
   propagation is in-flight? 30s? 60s? Should it be configurable via flag?

2. **Failure policy:** If a propagateWhen CEL expression fails to evaluate
   (type error, missing field), should propagation fail-open (allow the instance)
   or fail-closed (block it)? Recommendation: fail-closed, consistent with
   safety-first approach.

3. **Overlapping propagations:** When the RGD spec changes again while
   propagation is in-flight, should we immediately restart propagation with the
   new revision (abandoning in-flight progress)? Or wait for current propagation
   to complete? Recommendation: restart — the latest revision is always the
   desired state.

4. **Registry persistence:** Should the propagation registry (tracking which
   instances are cleared) be persisted, or rebuilt from instance
   RevisionReconciled conditions on controller restart? Recommendation: rebuild
   from conditions — they are the source of truth and this avoids persistence
   machinery.

## Discussion and notes

This proposal intentionally starts simple: periodic polling, no built-in batch
functions, no revision pinning. The goal is to establish the core primitives
(orchestrator, conditions, CEL environment, selective enqueue) that future
proposals can build upon.

The key invariant: **instances always converge to the latest revision**. The
orchestrator only controls pace, never destination. This means the system is
always making progress towards the desired state — propagation control adds
latency, not divergence.
