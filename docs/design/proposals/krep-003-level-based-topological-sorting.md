# KREP-003: Level-based Topological Sorting for ResourceGraphDefinitions

## Table of Contents

- [Problem statement](#problem-statement)
- [Proposal](#proposal)
- [Design details](#design-details)
   - [1. Level-based topological sorting using Kahn's algorithm](#1-level-based-topological-sorting-using-kahns-algorithm)
   - [2. Parallelization within levels](#2-parallelization-within-levels)
   - [3. Reverse-order deletion](#3-reverse-order-deletion)
   - [4. State synchronization between levels](#4-state-synchronization-between-levels)
   - [5. Level-based inventory management approach](#5-level-based-inventory-management-approach)
- [Other solutions considered](#other-solutions-considered)
- [Scoping](#scoping)
   - [In scope](#in-scope)
   - [Not in scope (deferred to follow-up work)](#not-in-scope-deferred-to-follow-up-work)
- [Testing strategy](#testing-strategy)
- [Requirements](#requirements)
- [Test plan](#test-plan)

---

## Problem statement

Currently, kro processes ResourceGraphDefinition instances sequentially,
applying resources one at a time in topological order. While this ensures
dependencies are satisfied, it's inefficient when multiple resources could be
created concurrently without violating dependency constraints.

ResourceGraphDefinitions can represent complex dependency graphs, including many
independent resources that don't depend on each other. The current sequential
approach leads to several limitations:

* **Sequential processing**: Resources are applied one by one, even when they
  have no dependencies on each other
* **Underutilized parallelism**: Independent resources that could be created
  simultaneously wait unnecessarily
* **Longer reconciliation times**: Large ResourceGraphDefinitions with many
  independent resources take longer than necessary to reconcile
* **Deletion inefficiency**: Resource deletion follows a similar sequential
  pattern, slowing down cleanup

The lack of parallelization means kro doesn't take full advantage of resources
that are independent within the dependency graph, leading to suboptimal
performance for these complex ResourceGraphDefinitions.

## Proposal

Move from sequential resource-by-resource processing to level-based
topological sorting with parallelized execution within each level.

### Overview

This proposal introduces a fundamental change in how kro processes
ResourceGraphDefinition instances:

1. **Level-based topological sorting** — Use Kahn's algorithm to compute
   dependency levels in the DAG
2. **Parallel execution within levels** — Process all resources within a level
   concurrently
3. **Sequential level progression** — Only advance to the next level after all
   resources in the current level are ready
4. **State synchronization between levels** — Synchronize runtime state after
   each level to enable cross-level references via CEL
5. **Reverse-order deletion and pruning** — Delete and prune resources from
   bottom to top, processing each level in parallel

Together, these changes improve reconciliation performance while maintaining
dependency safety and correctness.

---

## Design details

### 1. Level-based topological sorting using Kahn's algorithm

The DAG implementation (`pkg/graph/dag/dag.go`) now uses
`TopologicalSortLevels()` based on Kahn's algorithm, which computes dependency
levels rather than a flat ordering.

**Algorithm overview:**

```go
// TopologicalSortLevels returns the vertices of the graph grouped by topological levels
// using Kahn's algorithm. Each level contains vertices that have no dependencies on each
// other and can be processed in parallel. Within each level, vertices are sorted by their
// original Order for stability.
//
// For example, given a graph:
//
//	A -> C
//	B -> C
//	C -> D
//
// The result would be: [[A, B], [C], [D]]
// where A and B can be processed in parallel, then C, then D.
func (d *DirectedAcyclicGraph[T]) TopologicalSortLevels() ([][]T, error) {
}
```

**Properties of the level-based topological sort:**

* Returns `[][]string` representing levels
* All resources within a level have no dependencies on each other
* Resources in level _N_ may only depend on resources from levels 0 through _N-1_
* Enables parallel execution within each level
* Time complexity: _O(V + E)_ where _V_ is vertices and _E_ is edges

**Example:**

Given a dependency graph:
```
A → B → D
A → C → D
```

The algorithm produces:
```
Level 0: [A]
Level 1: [B, C]  // B and C can be created in parallel
Level 2: [D]
```

### 2. Parallelization within levels

Resources within the same level are processed concurrently using goroutines and
synchronization primitives:

**Concurrency control:**

```go
// Process all resources in the same level in parallel
var wg sync.WaitGroup
sem := make(chan struct{}, maxConcurrency)

for _, resourceID := range resourceIDs {
   wg.Add(1)
   go func() {
      defer wg.Done()
      sem <- struct{}{}
      defer func() { <-sem }()

      if err := processResource(ctx, resourceID); err != nil {
         processingErrorsMu.Lock()
         processingErrors[resourceID] = err
         processingErrorsMu.Unlock()
      }
   }()
}
```

**Thread safety:**

* Runtime state access is protected by mutexes (`pkg/runtime/runtime.go`)
* Mutable maps are guarded with `sync.RWMutex`

**Performance considerations:**

* Configurable concurrency limits (respecting existing controller flags)
* Error handling uses WaitGroup instead of errgroup to avoid early
  cancellation

### 3. Reverse-order deletion

Resource deletion mirrors the creation flow in reverse order, processing
topological levels from bottom to top (last level to first):

**Deletion flow:**

1. Compute topological levels (same as creation)
2. Reverse the level order
3. For each level (from last to first):
   - Delete all resources in the level in parallel
   - Validate all resources are fully deleted before proceeding to the next
     level

**Benefits:**

* Ensures dependents are deleted before dependencies
* Parallelizes deletion within each level
* Maintains referential integrity throughout cleanup
* Improves deletion performance for large ResourceGraphDefinitions

### 4. State synchronization between levels

After applying each level, the controller synchronizes runtime state to enable
CEL expressions in later levels to reference outputs from earlier resources:

```go
// After applying a level
if _, err := igr.runtime.Synchronize(); err != nil {
    return fmt.Errorf("failed to synchronize after level %d: %w", levelIdx, err)
}
```

This ensures that:
* Later levels can reference outputs from earlier levels via CEL
* Resource status and fields are up-to-date for dependency resolution
* Cross-resource references work correctly across levels

### 5. Level-based inventory management approach

**Status:** In progress (contributed by @jakobmoellerdev)

To fully support level-based topological sorting, we need an inventory
management approach that respects topological ordering. After investigating
ApplySet specification constraints, @bschaatsbergen and @jakobmoellerdev
identified that the current ApplySet specification cannot support multi-level
apply operations.

**Why ApplySets as-is are insufficient:**

The kubectl ApplySet specification ([KEP-3659](https://github.com/kubernetes/enhancements/blob/master/keps/sig-cli/3659-kubectl-apply-prune/README.md)) uses parent resource annotations for
inventory tracking (GKNN-based). This means:
- Only one ApplySet can be associated with a parent resource
- Multiple calls to `Apply()` on the same ApplySet don't distinguish between
  levels
- Pruning cannot respect topological order without breaking the spec

---

## Other solutions considered

| Option | Reason Rejected or Status |
|--------|----------------|
| Process all resources in parallel | Violates dependency constraints; resources may reference non-existent dependencies |
| Single ApplySet with multi-level apply | **Not feasible**: ApplySet spec uses unique parent annotations; cannot support multiple levels per parent |
| Multiple ApplySets per level | **Not feasible**: Violates ApplySet specification; parent annotations must be unique |
| Extending ApplySet specification | Requires upstream changes to kubectl ApplySet spec; long timeline; ConfigMap baseline demonstrates need first |
| Ad-hoc parallelization without levels | Difficult to reason about correctness; can't guarantee dependency satisfaction |
| DFS-based topological sort | Doesn't naturally group independent resources into levels; harder to parallelize |
| TBD | **Selected**: TBD |

---

## Scoping

### In scope

* Implement Kahn's algorithm for level-based topological sorting
* Parallel processing of resources within each level
* Reverse-order deletion with parallelization
* State synchronization between levels
* Thread-safe runtime state access
* **Level-based inventory management** (contributed by @jakobmoellerdev)
  - TBD

### Not in scope (deferred to follow-up work)

* Configurable concurrency limits (to be added after initial implementation)
* Migration path for existing ResourceGraphDefinitions
* Performance benchmarks and optimization (future work)
* Retry strategies for partial level failures
* Advanced error recovery mechanisms

---

## Testing strategy

### Requirements

* Kubernetes 1.30+
* Integration test suite with complex ResourceGraphDefinitions
* Test cases covering various dependency graphs

### Test plan

1. **Topological correctness:**
   - Verify Kahn's algorithm produces correct level ordering
   - Ensure resources within levels are truly independent
   - Test cycle detection returns appropriate errors
   - Validate level ordering is stable and deterministic

2. **Parallel execution:**
   - Confirm resources within a level are processed concurrently
   - Verify thread safety of runtime state access
   - Test that errors in one resource don't cancel others in the same level
   - Validate proper synchronization between levels

3. **Deletion order:**
   - Verify deletion processes levels in reverse order
   - Ensure dependents are deleted before dependencies
   - Test parallel deletion within levels
   - Confirm cleanup completes successfully

4. **State synchronization:**
   - Test CEL expressions that reference outputs from earlier levels
   - Verify runtime state is properly synchronized between levels
   - Ensure cross-resource references work correctly

5. **Edge cases:**
   - Single-level graphs (all resources independent)
   - Linear dependency chains (no parallelism possible)
   - Complex DAGs with multiple independent branches
   - Large ResourceGraphDefinitions (50+ resources)

6. **Level-based inventory management** (contributed by @jakobmoellerdev):
   - Verify migration diff computation for various state transitions
   - Test ConfigMap inventory tracking per level
   - Confirm pruning respects reverse topological order
   - Validate orphan resource cleanup
   - Test membership changes across reconciliation cycles
   - Verify ConfigMap creation, update, and deletion
