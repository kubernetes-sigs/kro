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

`time.now()` returns the current time. It is set once per reconcile. Every call in the same reconcile returns the same value.

The design splits into 2 parts. Comparing against now, and writing now to a field. They have different problems so they get different rules.

### 1. Comparing: requeue solving

`now()` returns a `KroTimestamp`. Subtracting 2 of them gives a `KroDuration`. These types only allow `+`, `-` and the 4 comparison operators. No casts, no accessor methods.

Every value stays in the form `a * now + b`. When a comparison runs we know both lines, so we know when they cross. That is the exact time the comparison flips. We requeue then.

```cel
${time.now() >= schema.metadata.creationTimestamp + duration("5m")}
```

This is false for 5 minutes after creation then true. We requeue at exactly creationTimestamp + 5m.

The restriction is the point. If we allowed `now().getHours()` or truncating to the day, the value would not be a line anymore and we could not say when it flips. Anything that is not a line is rejected at compile time.

This means kro does intervals and windows, not schedules. Comparing against a timestamp you already have works. Computing "9am today" or "every hour" from now does not. If you need a recurring schedule, pass the anchor in through the schema or use a CronJob.

Requeue is best effort. kro may be busy. Use these as guards, not as the thing that drives an action on a deadline.

### 2. Writing: stableString

You cannot write `now()` to a field directly. `${string(time.now())}` is not allowed.

The problem is every reconcile has a new `now()`. A field set to it changes every time, kro sees a diff, writes it, the write triggers a reconcile, and so on. `lastTransitionTime: ${string(time.now())}` is an infinite loop.

The fix is to say when the field is allowed to change.

```cel
createdAt:        ${time.now().stableString(once())}
lastUpgradedTime: ${time.now().stableString(whenChanged(db.version).onlyWhen(db.state == "active"))}
```

`once()` writes the first time and never again.

`whenChanged(a, b)` writes the first time and whenever the value of `a` or `b` differs from what it was at the last write.

`.onlyWhen(cond)` holds the write while `cond` is false. Nothing moves, including the record of what `a` and `b` were. When `cond` goes true a pending change is written then.

kro stores the last seen values of `a` and `b` in an annotation on the same object. The annotation and the field are written in the same apply. On a reconcile where nothing changed the output is byte identical and nothing is written.

The thing before `.stableString` is the instant to write. It can be offset.

```cel
expiresAt: ${(time.now() + duration("2160h")).stableString(once())}
```

The offset is frozen at write time. It does not slide.

An optional second argument is a Go time layout. Default is RFC3339.

```cel
date: ${time.now().stableString(once(), "2006-01-02")}
```

`once`, `whenChanged` and `onlyWhen` return a trigger. A trigger can only be passed to `stableString`. Writing one to a field is a compile error. `stableString` only works on `time.now()` plus or minus an offset. Together this means there is no way to write a timestamp that changes on every reconcile.

Not covered: "every N hours since the last write". That needs the field to compare against its own previous value. We left it out on purpose. The same self reference problem shows up in business hours and tickers. See alternatives.

## Examples

### Startup grace period

```cel
includeWhen:
  - ${time.now() >= schema.metadata.creationTimestamp + duration("5m")}
```

Requeues 5 minutes after creation.

### Certificate renewal guard

```cel
includeWhen:  # renew within 5d of a 90d expiry
  - ${time.now() >= timestamp(credential.spec.renewedTime) + duration("2160h") - duration("120h")}
```

Requeues 5 days before expiry. This is a guard. It decides if the renewal node is in the graph. It does not guarantee the renewal runs at that second.

### Certificate expiry

```cel
notAfter: ${(time.now() + duration("2160h")).stableString(once())}
```

Written at issue. Never changes.

### Last transition time

```cel
status:
  conditions:
    - type: Ready
      status: ${ready ? "True" : "False"}
      lastTransitionTime: ${time.now().stableString(whenChanged(ready))}
```

Changes when `ready` flips. Stays put otherwise.

### Last upgraded time

```cel
status:
  lastUpgradedTime: ${time.now().stableString(whenChanged(db.version).onlyWhen(db.state == "active"))}
```

Version goes 10.1 to 10.2 while upgrading. Nothing is written. State goes active. Written once. Version stays 10.2. Nothing more is written.

### Window opened

```cel
openedAt: ${time.now().stableString(whenChanged(window.open).onlyWhen(time.now() >= timestamp(window.open)))}
```

`window.open` comes from a ref or the schema. Records when the window actually opened for this instance. Requeues at `window.open`.

### Not supported

Business hours. `now().withTime({hours: 9})` is not a line. kro cannot know when it flips.

Tickers. "Write every hour" needs the field to read its own last value.

Propagation control with `duration("5m") * index`. The math is fine. `duration * int` and `indexOf` are not implemented yet.
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
They are the only way a Kro timestamp or duration can affect the result of a CEL expression.
Kro times cannot be cast to any other type. The only way out is `stableString`, which goes
through the executor and records no requeue.

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
propagateWhen:
  - ${time.now() >= timestamp(schema.spec.startAt) + duration("5s")}
```
with a `startAt` that some other controller bumps every few seconds.

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

Helpers that snap to a calendar boundary are out. `withTime({hours: 17}, tz)`, `addDays`,
`truncate`. They are constant for a stretch and then jump, so they are not a line and the
solver cannot say when a comparison on them flips. Supporting them would mean a second
requeue mechanism next to the solver. If you need a calendar anchor pass it in.

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

### No solver: greaterThanNow / lessThanNow

This is the main alternative. Drop the affine types and the solver. Ship 3 functions.

```cel
time.greaterThanNow(ts)
time.lessThanNow(ts)
time.now().stableString(trigger)
```

`greaterThanNow(ts)` is `ts > now`. `lessThanNow(ts)` is `ts < now`. `ts` is a plain CEL timestamp so you can use the whole standard library to build it. `stableString` is the same as above.

What this removes. There is no `KroTimestamp`, no operator rewrite, no "is this value still a line" rule. `now()` never leaks into a type the user has to learn. You can do `timestamp(x).getHours()` or anything else CEL already has and then compare. The compiler and the AST rewrite in Overriding go away. A lot less code.

What this gives up. We no longer know when a comparison flips, so we cannot requeue at that time. A gate like `greaterThanNow(creationTimestamp + duration("5m"))` is false until the next reconcile after 5 minutes. When that is depends on what else wakes the instance and the resync period. Changing the resync from 10s to 10h changes how every time gate behaves.

We could add a knob. `time.greaterThanNow(ts, requeue: true)` or a `requeueAt(ts)` function. That puts the requeue decision on the user, which is what the solver was built to avoid. It also brings back the fairness problem in a less controlled way because any timestamp can be asked for.

The trade is simplicity now against predictable gates later. The solver is more code but it is the only way "wait 5 minutes" means 5 minutes without the user saying so.

We went with the solver. The restriction it needs, everything stays a line, turned out to be the same restriction that keeps time from being misused. Schedules are out either way. The solver just makes the ones that are in reliable.

### explicit ask for next requeues

An alternative design could be having `time.now` and requiring the user
provide a direction `requeueAfter`. This adds extra complexity and area
for users to make mistakes.

If we can solve accurately, that is a much better user experience.