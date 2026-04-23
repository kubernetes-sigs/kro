# Propagation Rate Limit

## Problem

When a new GraphRevision becomes Active, the RGD controller re-registers the
instance microcontroller, which enqueues every existing instance for
re-reconciliation. Every instance resolves to the latest revision on its next
reconcile, and each reconcile applies every resource in the graph via
server-side apply.

The SSA patch itself is cheap. The problem is what it causes downstream. When
an RGD image field changes, every instance's Deployment spec changes, which
triggers a new ReplicaSet, which terminates old pods and schedules new ones,
which pulls images and fires readiness probes. For an RGD with 500 instances
and 3 Deployments each, that's 1500 concurrent pod rollouts hitting the
scheduler, the node autoscaler, and the image registry simultaneously.

There is no mechanism to pace how quickly instances begin transitioning. The
blast radius is always 100%.

## Proposal

Control propagation with a single parameter that defines how many instances are
admitted to a new revision per unit of time:

```yaml
metadata:
  annotations:
    kro.run/propagation-rate: "10/m"
```

Meaning: release up to 10 instances per minute. Every minute, the controller
releases the next batch of up to 10 pending instances. The downstream effects
(pod rollouts, scheduling, image pulls) are spread over time instead of hitting
all at once.

Format: `"<count>/<unit>"` where unit is `s` (second), `m` (minute), or `h`
(hour). When unset, all instances transition immediately (current behavior).

### Why time-based rate limiting

A concurrency limiter (max-in-flight) bounds how many instances are
mid-transition simultaneously. But kro's SSA apply completes fast — the
instance "lands" (label updates) within seconds. Slots free quickly and the
limiter burns through the entire fleet in minutes. The downstream effects (pod
scheduling, image pulls, node autoscaling) continue long after the slot was
freed. A concurrency limiter doesn't bound the actual cluster load.

What matters is the **admission rate** — how quickly new instances start
transitioning, regardless of how fast prior ones finish. A time-based rate
directly controls this: "my cluster can handle 10 instance transitions per
minute" is a statement about scheduler and node capacity, not about kro's
reconcile throughput.

### Configuration

**Annotation** on the RGD (per-RGD override):

```yaml
metadata:
  annotations:
    kro.run/propagation-rate: "10/m"
```

**Controller flag** (cluster-wide default when annotation is absent):

```
--propagation-default-rate=""
```

**Precedence:**

```
effective = annotation if present, else flag if non-empty, else unset

unset -> unlimited (current behavior)
```

The flag acts as a default, not a ceiling — the annotation always wins. A
ceiling flag can be added later without breaking changes (see Future work).

**Validation:**
- Format must be `<count>/<unit>` where count is a positive integer and unit is
  `s`, `m`, or `h`.
- Zero count, negative, non-numeric, or missing unit are invalid.
- Invalid annotations halt propagation and set `Propagated=Unknown` with reason
  `InvalidPropagationConfig`. Invalid flags prevent controller startup.

### Feature gate

`PropagationRateLimit` — Alpha, default disabled. When disabled: annotation is
ignored, flag has no effect, all instances transition immediately.

```go
const PropagationRateLimit featuregate.Feature = "PropagationRateLimit"
```

## Design

### Dispatch loop

The controller parses `"N/unit"` into a count and an interval. Every interval,
it releases up to count pending instances:

```
every interval:
    released_this_cycle = 0
    for each instance in propagation order:
        if released_this_cycle >= count: break
        if already on target: continue
        if already released: continue
        release instance
        released_this_cycle++

    if pending remain: requeue after interval
```

**Example** (`10/m`, 50 instances):
- T+0m: releases 10. 40 pending.
- T+1m: releases 10. 30 pending.
- T+2m: releases 10. 20 pending.
- T+3m: releases 10. 10 pending.
- T+4m: releases 10. 0 pending. Done.

All 50 instances transition over 5 minutes instead of simultaneously.

### Enqueue gating

The rate limit is enforced at the enqueue level. Only released instances are
enqueued for reconciliation — unreleased instances are never placed on the work
queue. Each dispatch cycle enqueues the next batch of released instances
directly.

This replaces the current behavior where re-registration enqueues all instances
at once. The RGD controller controls which instances enter the work queue and
when, based on the propagation rate.

Instance revision tracking (which revision each instance is currently on) is
handled by KREP-022 via the `graph` field on instances. This KREP reads that
data to determine which instances are pending and which have already
transitioned.

### Instance ordering

1. Sort by creation timestamp ascending (oldest first).
2. Namespace/name lexicographic tiebreaker.
3. Rotate by `targetRevision % len(instances)`.

The rotation distributes first-mover exposure across successive rollouts. The
order is computed once per target revision and reused across cycles.

### `Propagated` condition

A new condition on the RGD, always set regardless of feature gate:

- **True** (reason `AllPropagated`): all instances are on the target revision.
- **False** (reason `PropagationInProgress`): instances still transitioning.
  Message shows progress (e.g., "42/100 instances on revision 5").
- **Unknown** (reason `InvalidPropagationConfig`): invalid annotation value.

`Propagated` is outside the Ready hierarchy — an RGD can be `Ready=True` while
`Propagated=False`. The RGD is fully functional while instances are still
transitioning.

### Lifecycle

**New revision mid-rollout.** Target advances immediately. Instance order is
recomputed with the new rotation offset. Previously released instances that
haven't reached the old target re-enter the pending pool for the new target.

**Instance created during propagation.** New instances have no prior revision.
They resolve to the latest revision directly on first reconcile. They do not
enter the pending pool or consume rate budget.

**Instance deleted during propagation.** Skipped in the stored order. Does not
consume rate budget.

**Annotation changed mid-rollout.** New value takes effect on the next cycle
(triggered immediately by the RGD update event). The instance order is not
recomputed — only the rate changes.

**Controller restart.** In-memory state (released set, stored order) is lost.
The controller reconstructs from the informer cache using revision data from
KREP-022's `graph` field: instances whose revision matches the target are
treated as propagated, all others as pending. The first post-restart cycle
releases up to count instances. This may overshoot if instances had already
been released but not yet landed, but the overshoot is bounded by count and
occurs only once.

## Alternatives considered

**Concurrency limiter (max-in-flight).** Bounds simultaneous transitions, but
kro's SSA applies complete fast — slots free quickly and the limiter burns
through the fleet in minutes. Doesn't bound the downstream cluster load (pod
scheduling, image pulls) that continues after the apply completes.

**Per-RGD spec field.** Mixes operator concerns with the RGD definition.
Annotations keep these decoupled and avoid API surface growth. Changing
annotations does not trigger a new GraphRevision.

**Global flag only.** Forces all RGDs to the same rate. A 3-instance test RGD
and a 500-instance production RGD need different profiles.

**Annotation on GraphRevision.** GraphRevisions are immutable. The rate is an
operational parameter that should be changeable without issuing a new revision.

## Relationship to Instance Propagation Control

This proposal lays infrastructure (enqueue gating, `Propagated` condition,
propagation ordering) for the future Instance Propagation Control KREP, which
adds policy-driven strategies and CEL-based health gating via a `propagate`
struct on the RGD spec. The annotation introduced here acts as an
operator-imposed ceiling over that policy — even if the policy permits more,
the rate annotation caps throughput. Revision tracking is provided by KREP-022.

## Future work

- **Instance Propagation Control (KREP-024).** Policy-driven strategies and
  CEL-based health gating for instance propagation across GraphRevisions.
- **Burst parameter.** `kro.run/propagation-burst` to allow a larger initial
  wave before settling into the steady rate.
- **CEL-based rate expressions.** Allow custom rate definitions via CEL
  (e.g., exponential ramp-up, percentage-based pacing) instead of only
  the fixed `count/unit` format.
- **Ceiling flag.** Hard cluster-wide cap the annotation cannot exceed.

## Testing

**Unit tests:**
- Rate parsing: `"10/m"`, `"5/s"`, `"100/h"`, invalid formats.
- Precedence: annotation > flag > unset.
- Dispatch loop: releases up to count per cycle, stops when pending is empty.
- Instance ordering: timestamp sort, namespace/name tiebreaker, rotation offset.
- Sort-once: reused across cycles; deletion skips without recompute.
- New target revision: fresh sort, order recomputed.
- Enqueue gating: only released instances are enqueued.
- Restart: reconstruct from graph field, first cycle releases up to count.

**Integration tests:**
- Annotation set: verify releases per cycle match configured rate.
- No annotation: flag default applies.
- Invalid annotation: `Propagated=Unknown`, propagation halted.
- Rate `1/m`: single instance per cycle.
- Mid-rollout annotation change: new rate on next cycle.
- New revision mid-rollout: order recomputed, pending pool reset.
- Instance create/delete during propagation: correct behavior.
