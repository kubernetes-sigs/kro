# KREP-025: Time Functions for CEL

## Problem statement

An operator written in Go can easily check the current time and decide not to
make an update because it is outside of a deployment window. It can monitor the
duration between events and decide to take an action on that.

kro has no way to access time. The workaround today would be to have an external
system pass in the time through a schema value. This adds complexity and
potentially causes additional reconciles beyond what is necessary.

kro's CEL environment has limited time helpers. Date and time math is notoriously
difficult, and without the proper library to handle it, it is basically
impossible to get correct in CEL.

This primitive is orthogonal to the graph engine design (KREP-024). Both the RGD
model and the proposed Graph Kind want it, with no major difference in
implementation or consequences for either.

## Non goals

- Fully designing every possible helper someone might need in the time library.
  We can iterate on what is needed easily.
- Stopping the user from making bad decisions. We should make it easy to do the
  right thing and hard to do the wrong thing, but we aren't aiming to stop
  people from misusing or abusing the time package.

## Solution

We are proposing a CEL standard time library with restrictions on the types.
With the correct restrictions on the types we can solve for when reconciles 
should happen.

This CEL library will introduce new types called 

1. `KroTimestamp` returned by `now()`
2. `KroDuration` only accessible by subtracting two `KroTimestamp`

`now()` is set once per reconcile. Every call to `now()` will have the same value.

These timestamps are very similar to the CEL standard library Timestamp/Duration but they 
track if now() has been used to compute them. This is done to enable "solving" of requeue.

The CEL standard library is designed to always be able to figure out when we need to requeue.
This means our time values are intentionally limiting. Casting to an integer would remove our ability
to solve for the time, so we would not allow it.

As a compromise we will still allow casting to a ```string(time.now())``` for the purposes of setting 
last transition timestamp and other values. This cast will be inherently removing our ability to solve.
It's possible someone could do something like ```Timestamp(string(time.now())) > ...``` and write an incorrect
expression. To prevent this, we could require a special function that is clear you are opting out of time solving
as something explicit like ```time.now().toStringNoRequeue()```.

## Examples

### Startup grace period

Suppose we have a component that has a long startup period. As a result
of this it may be acceptable to ignore issues (for example in a custom status
condition) for a brief period after the object is created.

```cel
${time.now() >= schema.metadata.creationTimestamp + duration("5m")}
```

Solver will requeue after 5 minutes of creation timestamp.

### Certificate renewal

We can include a renewal job that will renew a certificate only if the
certificate is going to expire within 5 days.
```cel
includeWhen:  # renew within 5d of a 90d expiry
  - ${time.now() >= timestamp(credential.spec.renewedTime) + duration("2160h") - duration("120h")}
```

Solver will requeue 5 days before 90 day expiry.

Note that kro can't commit to a strong guarantee of when things get rescheduled.
It would be a bad idea to try to renew the certificate 5 seconds before it
expires, because kro may be busy with other work and so on. This strict
guarantee is nearly impossible in a K8s operator world.

### Business hours

Time window blockers are a good illustration of the power of this primitive.

Suppose you have a job that should run only during 9am-5pm, so the consequences
of the job failing can be dealt with during business hours.

```
includeWhen:
  - ${cel.bind(open,  time.now().withTime({hours: 9},  schema.spec.timezone),
     cel.bind(close, time.now().withTime({hours: 17}, schema.spec.timezone),
       time.now() < close
         ? time.now() >= open                                   # before/in window today
         : time.now() >= open.addDays(1, schema.spec.timezone)  # past close: gate on tomorrow's open
     ))}
```

The solver will requeue at the next time the window opens or closes.

Note this example assumes `KroTimestamps` have a `withTime`. The exact details of 
time helpers are not fully specified in this document.

### LastTransitionedTime

Time will allow converting to string for setting lastTransitionTime.

```
status:
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: ${string(time.now())}
```

No requeue will occur because of this.

### Propagation control

[Propagation control](https://github.com/kubernetes-sigs/kro/pull/861/changes) is a major use case for this time function.
Time is effectively a prerequisite for handling propagation control. 

One concept may be wanting to rollout a change to one instance every 5 minutes
```
resources:
  - id: deployments
    forEach:
      app: ${apps}
    # propagateWhen is AND-of-all (same as readyWhen). The gate opens when
    # enough wall-clock time has passed for THIS instance's slot in the order.
    propagateWhen:
      - >-
        ${time.now() >=
          timestamp(schema.metadata.annotations['kro.run/propagation-start'])
            + duration("5m") * indexOf(deployments, app)}
    template:
```

The exact propagation control design is not finalized but it's clear time will be useful.


## Implementation

### Basic Definitions

```
type KroTimestamp struct {
    NowCount    int
    TimeOffset  time.Duration
}

type KroDuration struct {
    NowCount int
    Duration time.Duration
}
```

A value represents the affine function `value(now) = NowCount * now + TimeOffset`
(or `Duration` for `KroDuration`). `TimeOffset` is the constant term, measured from the
Unix epoch, not a delta from the current time. A plain CEL `timestamp` `T` lifts to
`KroTimestamp{NowCount: 0, TimeOffset: T}` and a plain CEL `duration` `d` lifts to
`KroDuration{NowCount: 0, Duration: d}`.

Some examples
```
Now() -> KroTimestamp{NowCount: +1, TimeOffset: 0}

Now() + 1h -> KroTimestamp{NowCount: +1, TimeOffset: +time.Hour}
Now() - 1h -> KroTimestamp{NowCount: +1, TimeOffset: -time.Hour} 

# Adding timestamps in CEL errors. We replicate this.
Now() + Now() -> Error. Adding timestamps is not meaningful.

# Subtracting timestamps gives a duration in CEL. We replicate this too.
# Let T = timeOf("1/2/2029") as an epoch offset.
Now() - timeOf("1/2/2029") -> KroDuration{NowCount: +1, Duration: -T}   # now - T
timeOf("1/2/2029") - Now() -> KroDuration{NowCount: -1, Duration: +T}   # T - now

# a=difference between current time and 2029
# b=current time added to difference between current time and 2029. 
# b will increase in value faster than a
let a = Now() - timeOf("1/2/2029") = KroDuration{NowCount: +1, Duration: -T}
let b = Now() + a = KroTimestamp{NowCount: +2, TimeOffset: -T}           # 2*now - T
```

Definitions of operations

Addition
```
TSa + TSb   = ERROR   # timestamp + timestamp: no CEL overload; adding two instants is meaningless
TS  + Dur   = KroTimestamp{ NowCount: TS.NowCount + Dur.NowCount, TimeOffset: TS.TimeOffset + Dur.Duration }
Dura + Durb = KroDuration{ NowCount: Dura.NowCount + Durb.NowCount, Duration: Dura.Duration + Durb.Duration }
```

Subtraction
```
TSa - TSb   = KroDuration{ NowCount: TSa.NowCount - TSb.NowCount, Duration: TSa.TimeOffset - TSb.TimeOffset }
TS  - Dur   = KroTimestamp{ NowCount: TS.NowCount - Dur.NowCount, TimeOffset: TS.TimeOffset - Dur.Duration }
Dura - Durb = KroDuration{ NowCount: Dura.NowCount - Durb.NowCount, Duration: Dura.Duration - Durb.Duration }
Dur  - TS   = ERROR   # duration - timestamp: no CEL overload
```

Comparison
```
# Let now be the literal timestamp for the cel evaluation.
TSa < TSb => TSa.NowCount * now + TSa.TimeOffset < TSb.NowCount * now + TSb.TimeOffset
```

### Solving

To solve, we only need to consider every comparison operator `<`, `<=`, `>`, `>=`.
The comparison operators are the only way a Kro timestamp or a Kro duration is able to
affect the result of a CEL expression while staying inside the solver. Kro times are not
valid for Kubernetes objects and cannot be cast to any other type, with one exception:
`string()`.

`string()` is an explicit escape hatch. The moment a Kro time is converted to a string,
solving gives up on that value: no requeue is recorded for it, and anything derived from
the string (for example `timestamp(string(time.now())) > ...`) is an ordinary CEL value the
solver knows nothing about. This is the trade-off that lets `lastTransitionTime` and similar
fields be written. Every other exit from the Kro types is one of the 4 comparison operators.

Solving will happen as part of the evaluation of each comparison operator for Kro's time types.
This means we don't need to implement complicated static analysis that is hard to maintain.

For example, we will never execute the second part of this statement.
```
false && time.now() >= timestamp("20267-01-01T12:10:00Z")
```

We don't need to run any solving logic of any statements that do not execute. If the graph changes
then another reconcile will occur.

To actually solve for the critical time we need to reeval we can do the following math. 

For LHS < RHS, we can picture two lines
```
LHS:  y = LHS.NowCount · now + LHS.TimeOffset
RHS:  y = RHS.NowCount · now + RHS.TimeOffset
```

We can compute the intersection of these lines as
```
LHS.NowCount·now + LHS.TimeOffset = RHS.NowCount·now + RHS.TimeOffset
(LHS.NowCount − RHS.NowCount)·now = RHS.TimeOffset − LHS.TimeOffset
flipTime = (RHS.TimeOffset − LHS.TimeOffset) / (LHS.NowCount − RHS.NowCount)
```

Decide to requeue or not
```
if (LHS.NowCount - RHS.NowCount) == 0 { // Parallel lines. Never changing.
  return noRequeue
}

if flipTime > now { // Flip in the future, requeue then.
  return requeue(flipTime)
} else { // Already flipped in past. No need to requeue.
  return noRequeue
}
```

This same math generalizes for the duration. We take the earliest requeue time out of all comparisons.

Note the same formula handles a `NowCount` whose magnitude is greater than one (for example `b = Now() + a` has `NowCount == 2`): the denominator is simply non-zero, so `flipTime` is still solved normally. Such a value advances faster than the wall clock and is not a real clock instant, but the intersection math treats it uniformly and requeues at the computed `flipTime`.

### Overriding

CEL doesn't support [adding custom overloads to standard operators across types](https://github.com/cel-expr/cel-go/issues/252) (attempting it fails as a [singleton function incompatible with specialized overloads](https://github.com/cel-expr/cel-go/issues/990)).

So to support comparisons like
```now() > k8sObject.expirationTime```

we would need to have users cast values explicitly like 
```now() > KroTimestamp(k8sObject.expirationTime)``` 

or use a custom function like
```now().isAfter(k8sObject.expirationTime)```

A workaround we could do is automatically rewrite the CEL AST from the form
`now() > k8sObject.expirationTime` to `now().isAfter(k8sObject.expirationTime)`. 
This does require some effort but this is not a first for Kro. We already rewrite
the AST in custom status conditions to make the user interface more ergonomic. 

It's possible automatically casting values will be cleaner or another solution is possible.
This section is to highlight some complexities with this approach that will need 
to be worked out.

### Rollout plan

This feature will be behind an alpha feature gate much like ```omit``` default off.

The plan would be to set to default on after a couple of versions and positive community feedback.

### Fairness and infinite reconciles

Suppose a user writes
```
resources:
  - id: clock
    template:
      # ConfigMap — WRITE the new timestamp (desired state)
      apiVersion: v1
      kind: ConfigMap
      data:
        lastUpdatedTime: ${string(time.now())}

  - id: gated
    # READ the OBSERVED (previous) value → real age → valid gate
    propagateWhen:
      - ${time.now() - timestamp(clock.data.lastUpdatedTime) > duration("5s")}
```

We would requeue every 5 seconds and potentially slow down other instances reconciling 
by adding a backlog. 

One idea would be to prevent casting to a string, but this isn't the only way a user could
abuse time to reconcile every so often.

To prevent this we need some way for Kro admins to put controls on this. The simplest option would
be a ```--min-time-solver-requeue=10m```. Any time an instance tries to requeue faster than that it 
gets delayed to the minimum.

This document proposes a per instance (or graph instance) token bucket rate limiter to give more flexibility.
```
lim := buckets.get(instanceKey)          // rate.NewLimiter(1/300s, 5): 5 burst, refill 1 per 5min
if lim.Allow() {
    return RequeueNeededAfter(d)          // token available → honor the time requeue
} else {
    // over budget → push the requeue out to when the next token is available
    return RequeueNeededAfter(max(d, lim.Reserve().Delay()))
}
```

Configured with
```
--instance-time-requeue-burst=3
--instance-time-requeue-refill-interval=10m
```

This would be in memory so Kro restarts would reset the token bucket.


## Other time functions

This document does not describe other time helpers to reduce scope.

Most time helpers should fit very naturally into the described library.

Certain helpers need a little extra machinery. `time.now().withTime({hours: 17}, tz)` is not
affine in `now()`: it is constant for the whole day and then jumps at local midnight. Modelled
as a plain `KroTimestamp` it is correct until midnight and silently stale after. This case is
easy to handle: a helper that snaps to a calendar boundary records the next instant at which its
value changes (here, the next midnight in `tz`), and the solver requeues at the earlier of that
instant and the comparison's `flipTime`. The same rule covers `addDays` on `now()` (jumps at DST
transitions) and helpers like `nextBusinessDay` (jumps at midnight). This is a conservative
requeue: kro may wake, re-evaluate, and find nothing changed.

Helpers whose value changes at fine granularity, for example `time.now().getSeconds()` or
truncating `now()` to the second, are not hard to model but are hard to support well: the value
genuinely changes every second, so any gate built on it wants to requeue every second. These
should be rejected or restricted rather than solved. The follow-up KREP should decide which
helpers to ship with this in mind.

If this KREP is accepted, a follow up KREP can be written to go over potential helpers and usefulness. 

## Alternatives

### Not having time in CEL

Time adds potential for tons of foot guns and complexity. We remove an
assumption that we can calculate everything based on just inputs to the graph.

Most of these examples can be solved with another operator or a cronjob or some
other resource. While this is true it ends up being workarounds for kro's lack
of feature support. kro becomes more powerful with the option of using this.

Time is also a core building block. Features like propagation control and so on
could greatly benefit from having a way to represent time.

### time.now() no requeue

The simplest thing beyond doing nothing could be having time have no impact on
requeuing.

Writing correct applications without the ability to control the next time kro
should evaluate the time is basically impossible. For example, the time window
opening and closing becomes impossible to debug if it is just whenever kro
reconciles next. It could happen quickly or after a long time.

Well written RGDs should not be random. A goal is that changing the default
requeue period from 10 seconds to 10 hours should not change how user RGDs
mostly behave.

### explicit ask for next requeues

An alternative design could be having `time.now` and requiring the user
provide a direction `requeueAfter`. This adds extra complexity and area
for users to make mistakes.

If we can solve accurately, that is a much better user experience.