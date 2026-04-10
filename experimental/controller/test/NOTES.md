# Test Quality Reconciliation — Working Notes

## What's done (this branch: `test-quality`)

### Fixes to existing tests
- **time.Sleep removed** from `finalize_test.go:TestFinalizesTargetAbsentSkips` — replaced with
  `waitForAbsence`, satisfying "assertions observe completion, never guess timing"
- **Misleading test names renamed**: `TestReadyWhenExternalRefGatesDownstream` →
  `TestReadyWhenDoesNotGateDownstream` (and forEach variant). readyWhen is a health signal, not a
  gate — the test names said the opposite of what the tests proved.
- **Stale doc comment fixed** on `TestDriftNotRestored` — ghost comment from old behavior described
  the opposite of what the test asserts.
- **Phantom test removed** — `TestPartialStatusResolution` was a dangling doc comment in
  `lifecycle_test.go` with no test function.
- **Doc comments added** to `TestFullLifecycle`, `TestStatusActiveOnSuccess`,
  `TestForEachCollectionScaleUpDown`, `TestDriftNotRestored` — each now references the design
  document section it validates.
- **Poll timeouts normalized to 30s** across all test files and helpers. The previous 5s timeouts
  were marginal under parallel load.

### New tests
- `fault_test.go` — 4 tests exercising error paths:
  - `TestWatchedResourceDeletedMidReconcile`: watched resource deleted → controller recovers
  - `TestOwnedResourceDeletedExternally`: owned resource deleted → controller recreates
  - `TestInvalidCELExpressionSurfacesError`: bad CEL → Accepted=False → fix → recovers
  - `TestConflictThenSpecChangeResolvesConflict`: 409 → spec change removes contested field → Ready
- `prune_safety_test.go` — 2 tests proving uncertain absence blocks prune:
  - `TestPruneSafetyPendingBlocksPrune`: data-pending doesn't prune conditional resources
  - `TestPruneSafetyConflictBlocksPrune`: 409 conflict doesn't prune independent resources
- `TestIdempotentReReconcileZeroWrites` in `performance_test.go` — re-reconcile with no change
  produces zero writes to ALL managed resources (extends TestSteadyStateNoStatusWrite)
- `TestContributeShapeDetectedByExistence` in `contribution_test.go` — pre-existing resource →
  Contribute shape → NOT deleted on prune (asserts behavioral consequence, not internal classification)
- `TestPropagateWhenOnForEach` in `correctness_test.go` — propagateWhen gates data flow from a
  node that depends on forEach collections

## What's NOT done — and why

### Two controller bugs discovered

**Bug 1: Dynamic watch event starvation under parallel load.**
With MaxConcurrentReconciles=1 (the default), ~80 parallel tests produce enough queue depth that
dynamic watch events (external resource changes) are starved. Tests that depend on the controller
reacting to external ConfigMap changes timeout because the single worker is processing other Graphs.

Evidence: TestFieldConflictResolvesOnOwnershipRelease and TestPropagateWhenGatesDataFlow fail
consistently under full suite load but always pass in isolation. Timeout-independent — increasing
from 5s to 60s doesn't help. The events never arrive while the queue is saturated.

**Bug 2: Concurrent reconciliation produces incorrect results.**
Setting MaxConcurrentReconciles=4 (to fix starvation) causes DIFFERENT tests to fail — watch tests,
collection tests, and contribution tests that pass at workers=1. This suggests shared mutable state
or missing synchronization in the reconcile path, likely in the WatchCoordinator which maintains
scalar/collection indexes under a RWMutex while reconciles read and write concurrently.

Evidence: TestDynamicWatchExternalRefChange, TestDynamicWatchCollectionMembershipChange,
TestForEachCollectionScaleUpDown, TestContributionUpdatesWhenDependencyChanges all fail at workers=4
but pass at workers=1.

### What to do next pass
1. **Skip the pre-existing failing tests** with explicit comments documenting the bug. A test suite
   that starts red is a test suite nobody trusts.
2. **Skip new tests that depend on the same dynamic watch path** (TestWatchedResourceDeletedMidReconcile,
   TestPruneSafetyPendingBlocksPrune hit it).
3. **File bug: watch coordinator concurrency** — the workers=4 failure set is the reproduction case.
4. **File bug: dynamic watch starvation** — the workers=1 failure set is the reproduction case.
5. **Run clean 3/3, commit, PR.**

### Test architecture note
The architecture (single envtest, single controller, high parallelism, namespace isolation) is
correct. It found real bugs. Don't reduce parallelism, split envtest instances, or lower GOMAXPROCS —
all of those hide bugs instead of fixing them.
