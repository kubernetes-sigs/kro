# KREP-025: Time Functions for CEL

## Problem statement

kro has no way to interact with time. Here are 4 example use cases people want to build that need it.

**Wait before trusting a signal.** A component takes a few minutes to start and reports
unhealthy while it does. The RGD wants to ignore that condition for 5 minutes after the
object is created, then start caring.

**Renew a certificate before it expires.** The cert is good for 90 days. A renewal job
should enter the graph 5 days before the end and not before.

**Record when something happened.** A database upgraded from 10.1 to 10.2. Users want
`lastUpgradedTime` on the status. It should be set once when the upgrade lands and never
touched again until the next upgrade.

**Roll out in waves.** 10 instances, 1 every 5 minutes. Add some bake time to avoid big bang
changes with high risk.

The workaround today is to have something outside kro write the time into the schema.
It is complex and doesn't scale well. Every instance of every RGD that wants a clock
needs that controller to find it, write to it, and do so often enough that the gate
flips close to when it should, which means the write rate grows with instance count and
most of those writes change nothing.

### Why this is hard in CEL

**Time is not deterministic.** A graph/RGD with a time gate in it behaves differently depending
on when it happens to run. If we just add `now()`, a gate like "5 minutes after creation" flips 
whenever kro next looks, which can be the resync period or whenever something unrelated changes.
This would be a very bad experience if a 5 minute grace period turns into a 12 hour one.
However time is handled, the end user behavior should be very similar to if we 
requeued the graph/RGD every 5 seconds or if we reconciled the graph every 24 hours.

**Writing time means reading our own write.** kro reconciles when anything it watches
changes, including its own writes. `lastUpgradedTime: ${string(time.now())}` gets a new
value every reconcile, kro writes it, the write wakes the instance, and it loops. If Kro
expressions are not idempotent we will likely end up in infinite reconciles.

**kro only sees current state.** A CEL expression can read the desired state of every node
in the graph. It cannot read what is in the cluster right now for the node it is rendering,
and it cannot read what that node wrote on the last reconcile. So `has(prev) ? prev : now()`
is not something you can write. The previous value is not in scope. 

## Non goals

- Fully designing every possible helper someone might need in the time library.
  The goal is to focus on the hard parts of time in kro then exact helpers and details 
  can be iterated on.
- Preventing users from intentionally writing graphs/RGDs that cause infinite reconciles. 
  Users can write expressions today that do this and time will necessarily be another way to do this.

## Solution

This design applies the same to graph as it does to RGD.

We are introducing
1. A new function `time.now()` which returns a value of type `KroTimestamp`
2. An idempotent way to convert a `KroTimestamp` to a string, with `time.now().stableString(...)`

More details will follow the examples.

## Examples

### Startup grace period

```cel
includeWhen:
  - ${time.now() >= schema.metadata.creationTimestamp + duration("5m")}
```

Includes a node after 5 minutes past creationTimestamp. 

### Certificate renewal guard

```cel
includeWhen:  # renew within 5d of a 90d expiry
  - ${time.now() >= timestamp(credential.spec.renewedTime) + duration("2160h") - duration("120h")}
```

Includes a node 5 days before a cert is going to expire.

### Certificate expiry

```cel
createdTime: ${(time.now()).stableString(once())}
```

Writes the creation timestamp. The first time this field evaluates the timestamp will be saved.

All future evaluations use the previous value.

### Last transition time

```cel
status:
  conditions:
    - type: Ready
      status: ${ready ? "True" : "False"}
      lastTransitionTime: ${time.now().stableString(whenChanged(ready))}
```

Writes the last transitioned timestamp. If `ready` does not change it will use the stored
value. If `ready` does change it will store a timestamp and use that.

### Last upgraded time

```cel
status:
  lastUpgradedTime: ${time.now().stableString(whenChanged(db.version).onlyWhen(db.state == "active"))}
```

Gets the value of the last time a database upgrade happened successfully.

If the database is not active it will not update the timestamp and just use the stored value.

### Business hours

Only run a job between 9am and 5pm so failures land during the day.

```cel
includeWhen:
  - ${cel.bind(open,  time.now().withTime({hours: 9},  schema.spec.timezone),
     cel.bind(close, time.now().withTime({hours: 17}, schema.spec.timezone),
       time.now() < close
         ? time.now() >= open                                   # before/in window today
         : time.now() >= open.addDays(1, schema.spec.timezone)  # past close: gate on tomorrow's open
     ))}
```

Requeues at the next open or close. Before 9 it requeues at `open`. Inside the window at
`close`. After close at tomorrow's `open`.

This needs `withTime` and `addDays`, which are not in this KREP. Within one reconcile they
return a fixed instant so the comparisons solve normally. Across reconciles they jump at
midnight, which the solver cannot see on its own. See Other time functions for how they
would be supported. Until then pass `open` and `close` in through the schema and kro gates
on them correctly today.

### Propagation control

Roll out one instance every 5 minutes. Each instance gets its slot offset from
the data that builds the list, so the gate is a plain line in `now`.

```cel
resources:
  - id: deployments
    forEach:
      app: ${apps}            # each app carries a precomputed slot, e.g. "0s", "5m", "10m"
    propagateWhen:
      - ${time.now() >= timestamp(schema.metadata.annotations['kro.run/propagation-start'])
                        + duration(app.slot)}
    template: ...
```

The design of propagation control is pending to another KREP so this is more of an example to show 
how time could benefit it.

## Details

This document describes an approach that applies equally between graph and RGD.

The first section will describe a CEL time library that is designed to allow kro to always know 
when it should requeue the graph/RGD next. The library is designed so for every expression a user
can express with time, we know when in the future the outputs of the CEL expressions will change.

The second section will describe how we can convert values from the CEL time library to strings
safely within Kro's constraints.


### 1. CEL timestamp and knowing when to requeue

#### Basic Definitions

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

#### Knowing when to requeue

To know when to requeue we only need to consider each comparison operator `<`, `<=`, `>`, `>=`.
They are the only way a Kro timestamp or duration can affect the result of a CEL expression. Every other
operator defined on KroTimestamp or KroDuration will result in another KroTimestamp or KroDuration.
Kro times cannot be cast to any other type or written to a Kubernetes object. The only other out is 
`stableString` (described in next section).

We will call the process of evaluating expressions and figuring out when the expressions will change as 
"solving".

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

#### Overriding

CEL does not let you add overloads to the standard operators across types, so
`time.now() > ts` would normally need a cast or a method call. We declare the kro types
on the standard operators for the type checker, so the full set of legal pairings is
known at compile time and anything else is a check error. At runtime a small program
decorator handles the one case CEL dispatch gets wrong, a kro value on the right hand
side, by mirroring the operation onto the kro operand.

### 2. Converting time to string with stableString

You cannot write `now()` to a field directly. `${string(time.now())}` is not allowed. Every
reconcile has a new `now()`, so the field changes every time, kro writes it, the write
wakes the instance, and it loops.

The fix is to only allow conversions in an idempotent way.

#### Triggers

`stableString` takes a trigger. A trigger says when the field may take a new value.

`time.now().stableString(once())` writes the first time and never again. As long as the expression
and the field it lives on do not change, the value is stable forever. Editing the expression or
renaming the field resets it, because the annotation key is derived from both. For example the following expression will only set its timestamp
after `willUpdateAfterSomeTime` is true.

```${spec.schema.willUpdateAfterSomeTime && time.now().stableString(once()) != ""}```

`time.now().stableString(whenChanged(a, b, ...))` writes the first time and whenever any of `a`, `b` differs from
what it was at the last write.

`.onlyWhen(cond)` on a trigger prevents writing while `cond` is false. The record of what the keys were
isn't updated when `onlyWhen` is false. When `cond` goes true a pending change is written then.
Several `onlyWhen` chain as AND. If `onlyWhen` is false for the first write the timestamp will be null.

#### Format

We will support an optional format method matching the CEL std library.

```cel
date: ${time.now().stableString(once()).format("2006-01-02")}
```

#### Implementation

In order to support this we need to store state to know what the previous value was.

The natural place to do this is on an annotation on the Kubernetes object of the 
resource it is defined on. Note we will disallow setting stableString in identity fields
and only allow it in node types Kro writes (template, patch, RGD nodes) to simplify the problem space.

The annotations will be of the form
```
internal.kro.run/keyHASH=valueHASH
internal.kro.run/keyHASH-timestamp=2026-01-02
```

The `keyHASH` is a hash of elements of the expression uniquely needed to identify it. For example suppose we have 
```
spec: 
  date: ${time.now().stableString(whenChanged(a, b))}
```

The hash could be a combination of field path where the expression lives (```spec.data```) + 
expression itself (```${time.now().stableString(whenChanged(a, b))}```) +
position in expression (```a```). Note we would have another annotation for ```b```.

The value hash would be a hash of the CEL value. It's possible we may need to constrain certain
values in CEL to ensure they are all hashable.

All hashes not defined each reconcile will be removed from an object.

The storing mechanism makes ```once``` trivial. If the timestamp annotation exists we
return it, otherwise we write ```now()``` to it and return that.

```whenChanged``` adds one check. We hash each key's current value and compare against the
stored hashes, and if any differ we write ```now()``` and update the hashes together.

```onlyWhen``` is evaluated first. If it is false we return the stored timestamp and touch
no annotation, so a change that happens while gated is still a change when the gate opens.

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

Forbidding `string(time.now())` closes one door, but it isn't the only way a user could
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

Helpers that snap to a calendar boundary are not in this KREP. `withTime({hours: 17}, tz)`,
`addDays`, `truncate`. They are constant for a stretch and then jump, so they are not a line
and the solver cannot say when a comparison on them flips. They are rejected at compile time
today.

We may need an approach where the helper records the next instant its value
changes (the next midnight in `tz`, the next DST transition) and the solver requeues at the
earlier of that instant and the comparison's flip. This would be conservative. kro may wake and find
nothing changed. It is a second requeue path next to the line solver and wants its own KREP.

Helpers whose value changes at fine granularity, for example `time.now().getSeconds()` or
truncating `now()` to the second, are not hard to model but are hard to support well: the value
genuinely changes every second, so any gate built on it wants to requeue every second. These
should be rejected or restricted rather than solved. The follow-up KREP should decide which
helpers to ship with this in mind.

If this KREP is accepted, a follow up KREP can be written to go over potential helpers and usefulness. 

## Alternatives

### Not having time in CEL

Time adds potential for tons of footguns and complexity. We remove an
assumption that we can calculate everything based on just inputs to the graph.

Most of these examples can be solved with another operator or a cronjob or some
other resource. While this is true it ends up being workarounds for kro's lack
of feature support. kro becomes more powerful with the option of using this.

Time is also a core building block. Features like propagation control and so on
could greatly benefit from having a way to represent time.

### functions instead of ```time.now``` like `greaterThanNow` / `lessThanNow`

Suppose instead of exposing ```time.now()``` we expose comparison helpers.

```cel
time.greaterThanNow(ts)
time.lessThanNow(ts)
time.stableString(trigger)
```

`greaterThanNow(ts)` is `ts > now`. `lessThanNow(ts)` is `ts < now`. `ts` is a plain CEL timestamp 
so you can use the whole standard library to build it. `stableString` is the same as above.

This switch to functions removes a lot of complexity with the implementation. 
We no longer need to implement ```KroTimestamp``` or ```KroDuration```. Solving 
becomes trivial. We just can look at the timestamp passed in to `time.greater/lessThanNow`
and do a one line comparison instead of math. We also no longer have to worry about working 
around CEL's inability to handle ```Timestamp > KroTimestamp```.

Another benefit is our API surface becomes more obvious. It's very clear from these three functions 
that the way to write a timestamp is with `stableString`. A user might assume they can just do `string(time.now())`
and see it is forbidden. 

This design would work but has a few disadvantages that made the proposed design the one we chose. 

1. We can't write future timestamps as easily. The proposed design can do `(time.now() + '24h').StableString(...)`
2. `time.now()` allows greater expressibility. An expression like `time.now() + 5m > expiration` could be expressed but would need to be modified to fit into the three functions.
3. `time.now()` is the most natural interface. It aligns with what other programming languages do and what the default expectation around time is for Kro.
4. The idea that kro could reverse engineer your CEL expressions and identify when requeues occur is a surprising and delightful feature. While surprising features are often seen as a negative, it is a positive surprise that Kro can figure this out and behave correctly for any expression written. 

### generic way to refer to previous state 

```StableString``` is essentially creating a narrow way to refer to previous state in Kro. It is reasonable to ask if we 
should instead just allow users to be able to refer to any previous state like ```object.previous.value``` and let users 
implement it.

This may be a reasonable approach and deserves a separate KREP but it does not appear to solve this issue.

Even if we do have some way to refer to a previous value, we would struggle to enforce users write idempotent expressions.
They may technically be able to check the previous state and only update the timestamp if a value changes, but it would be hard
to ensure that they actually are doing that in a way that doesn't cause infinite reconciles.

The second issue is around correctness. It's possible for the ```object.previous``` to change between when we read the object
and write the expression. This leads to a whole class of bugs that will be hard to deal with. Time can minimize a lot of these.
If we double write a timestamp the consequences are less severe than if certain jobs are being run twice. It is likely
that a general purpose `object.previous` mechanism could end up being very difficult to use correctly.

### explicit ask for next requeues

An alternative design could be having `time.now` and requiring the user
provide a direction `requeueAfter`. This adds extra complexity and area
for users to make mistakes.

If we can solve accurately, that is a much better user experience.