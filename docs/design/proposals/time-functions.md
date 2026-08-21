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

We are proposing a CEL standard library. The most interesting and novel aspect
of this is the impure section, a single `time.now()` function. It always takes
one argument.

`time.now(null)` returns the current timestamp. This form will not cause the
instances of the RGD to have any additional reconciles. There is no guarantee of
when it will be updated or how stale it can be.

`time.now(evaluateAfter)` also returns the current timestamp, and additionally
causes the instance to be reconciled after `evaluateAfter`. If the CEL
evaluation time has already passed `evaluateAfter`, the argument is ignored and
no special scheduling consideration is given.

Both forms return the standard CEL
[`timestamp`](https://github.com/google/cel-spec/blob/master/doc/langdef.md)
type, the same type as `schema.metadata.creationTimestamp`, so the result
composes with `duration()` arithmetic and the comparison operators used in the
examples.

## Examples

### Startup grace period

To illustrate how this works, here is a condition that has a 5 minute grace
period. Suppose we have a component that has a long startup period. As a result
of this it may be acceptable to ignore issues (for example in a custom status
condition) for a brief period after the object is created.

```cel
${time.now(schema.metadata.creationTimestamp + duration("5m"))
  >= schema.metadata.creationTimestamp + duration("5m")}
```

### Periodic rollout

A ticker rolls a workload on a fixed interval, for example to restart the pods
every 10 minutes to work around a memory leak.

The ticker is built by passing `time.now()` into itself with an offset added,
which schedules the next reconcile one interval out:
```cel
${time.now(time.now(null) + duration("10m"))}
```

To actually roll the pods we put a value that changes once per interval into the
pod template. A rolling restart happens whenever the pod template changes, so we
set an annotation to a monotonic bucket number: the current time divided by the
interval.
```cel
# pod template annotation; bumps once per 10-minute bucket and rolls the pods
spec:
  template:
    metadata:
      annotations:
        kro.run/restart-bucket: ${string(int(time.now(time.now(null) + duration("10m"))) / 600)}
```

`int(time.now(null))` is the current time in seconds since the epoch. Dividing by
the interval in seconds (600 for 10 minutes) gives a counter that increments by
one every interval and never repeats.

### Certificate renewal

We can include a renewal job that will renew a certificate only if the
certificate is going to expire within 5 days.
```cel
includeWhen:  # renew within 5d of a 90d expiry
  - ${cel.bind(renewAt,
        timestamp(credential.spec.renewedTime) + duration("2160h") - duration("120h"),
        time.now(renewAt) >= renewAt)}
```

Note that kro can't commit to a strong guarantee of when things get rescheduled.
It would be a bad idea to try to renew the certificate 5 seconds before it
expires, because kro may be busy with other work and so on. This strict
guarantee is nearly impossible in a K8s operator world.

### Business hours

Time window blockers are a good illustration of the power of this primitive.

Suppose you have a job that should run only during 9am-5pm, so the consequences
of the job failing can be dealt with during business hours. First we define a
ConfigMap (note this can and should be just kro variables after the variables
KREP is merged).

```yaml
# ConfigMap (id: clock) computing the window boundaries
data:
  nextWindowOpen: ${cel.bind(now, time.now(null),
    cel.bind(o, now.withTime({hours: 9}, schema.spec.timezone),
      cel.bind(nextOpen, now < o ? o : o.addDays(1, schema.spec.timezone),
        cel.bind(_, time.now(nextOpen), string(nextOpen)))))}
  nextWindowClose: ${cel.bind(now, time.now(null),
    cel.bind(c, now.withTime({hours: 17}, schema.spec.timezone),
      cel.bind(nextClose, now < c ? c : c.addDays(1, schema.spec.timezone),
        cel.bind(_, time.now(nextClose), string(nextClose)))))}
```

This ConfigMap uses the current time and some time helpers to tell us the next
time the window opens and the next time it closes.

Either we are inside the window or outside it. Reading the timeline from now,
whichever boundary comes first tells us which case we are in
(`O` = window opens, `C` = window closes):

```
Case 1: inside the window (business hours)

    O(9am)     C(5pm)                      O(next 9am)
  ───●━━━ now ━━━●───────────────────────────●──────────▶
                 ↑ next close is sooner than next open
     nextClose < nextOpen   =>   inside   =>   include


Case 2: outside the window (after hours)

    O(9am)     C(5pm)              O(next 9am)   C(next 5pm)
  ───●──────────●━━━━━ now ━━━━━━━━━●─────────────●──────▶
                                    ↑ next open is sooner than next close
     nextOpen < nextClose   =>   outside   =>   exclude
```

When the next close comes before the next open we are inside the window, which
is exactly what the includeWhen checks:
```cel
includeWhen:
  - ${timestamp(clock.data.nextWindowClose) < timestamp(clock.data.nextWindowOpen)}
```

Our requeue ends up being the sooner of the two, so we will reevaluate then.

(If this ends up being a common use case, a CEL helper like
`time.inWindow(open, close, tz)` could be added that follows this same logic.
This example is included to show the usefulness of `time.now()` and the ability
to build it into complex tools.)

## Implementation

Once per instance reconcile the runtime takes a timestamp and binds it into the
CEL activation as the `time` variable; the library's `now()` function reads that
bound value. All the `now()` calls in a given reconciliation are guaranteed to
get the same exact value.

The library records every future `evaluateAfter` call into a `RequeueCollector`
that was injected alongside the timestamp and shared by pointer with the
runtime. The CEL result carries only the timestamp; the earliest requeue instant
travels back through that shared collector, which the runtime exposes via
`EarliestRequeue()` and the controller translates into a `RequeueAfter` on the
reconcile result.

If the controller restarts we lose all the information about when to requeue.
This is ok because we will reconcile every instance again and compute when they
next need to requeue.

## Limitations

- Can't stop infinite reconciles. A user can easily write an RGD that requeues
  every second. This is true without the time primitive; users write RGDs, or
  RGDs within RGDs, that end up infinitely looping too by changing a resource
  they are watching.

## Other time functions

This section on pure time helpers is intentionally brief. Once we align on the
hard part of time (getting timestamps into CEL) we can work towards building
more helpers to manipulate and calculate time.

These functions are included because they were needed to make the examples
viable. They do not necessarily need to block the rest of the KREP.

`ts.withTime({hours, minutes, seconds, nanos}, tz) -> timestamp` sets the
time-of-day on `ts`'s calendar date in the IANA timezone `tz`. Any field left
out defaults to 0 and the result is DST-safe.

`ts.addDays(n, tz) -> timestamp` adds `n` calendar days to `ts` in the IANA
timezone `tz`, keeping the wall-clock time stable across DST transitions.

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

### decouple requeue and time.now()

An alternative design could be having `time.now` and a separate
`time.requeueAfter` to give finer-grained control.

The issue with this solution is it allows RGD authors to easily ignore the
concept of requeuing. Time becomes a potential for hard-to-debug unexpected
behavior without thinking through requeue. By forcing every call of time to
declare when it thinks it should be evaluated next we bring this front and
center.

### time.after(evaluateTime)

This was my initial thinking for `time.now()`. `time.after` would return false
before the time specified then true afterwards. It would requeue after the
evaluation time.

It is a hard-to-misuse primitive but lacks power. The ability to pass
`time.now()` to itself gives the ability to build tickers and on-off cycles like
those specified in the examples.

### Offset instead of a moment

`time.now` could take an offset such as `duration("5m")` as its argument to know
when to requeue, rather than an absolute instant. This makes some things easier
to represent; the ticker becomes more obvious. It makes others more difficult to
represent, like the time window blockers.

Overall we are not strongly opinionated here. It is possible we could take both.

### Time now solver

A solver. We could analyze CEL expressions and attempt to realize when
expressions flip. For example the expression `now() > startDate` could be parsed
and analyzed to understand `startDate` here is a critical time that we need to
requeue at. It's clever and would be a magical, great user experience if it
could work.

There is nothing to solve for in the example of setting an environment variable
to `time.now()`. When should this be requeued? A solver would say never. The
proposed design is asking the user to explicitly decide. Making the user make an
explicit choice avoids a false sense of security that we are able to figure out
what users intended from their RGD.

Solving is complicated. Every function a user passes into time needs a backwards
model. For example, if we call `someFunction(now()) > x` the only way to know
when we need to requeue is to know at what time `someFunction(now()) > x` becomes
true. At best we are on the hook to support this logic for every function we add
that takes time.

Solving is impossible in some situations. Even if we assume a perfectly
implemented solver evaluating `someFunction(now()) > x`, if `someFunction` is
sha256 no backwards model exists. We lose the ability to realistically run
solving logic on these.

The issue with this approach is not that we cannot cover every case, but that
from the RGD author's perspective the solver handling a case and not handling it
look identical. There are no error messages; your RGD just won't work and will
be really hard to debug.
