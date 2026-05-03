# Skill: simplify

# Simplify

This skill measures coupling and cohesion in code, weighted by architectural importance, and fixes
the worst violation. Each invocation works one scope — a package, a file cluster, a type. Fix the
worst violation, stop. Repeated invocations converge.

Approach with an adversarial frame of mind — measure as if trying to find the worst thing, not confirm the code is fine.

## Measuring coupling

Identify every piece of mutable state in the scope — struct fields, map entries, variables captured
by closures. For each: write sites (file:line), read sites (file:line), reset sites (file:line) or
"never reset."

Group files by shared mutable state into **interaction clusters** — files that read AND write the
same state belong to the same cluster. A file may participate in multiple clusters when it serves
multiple concerns.

**Record:**
- Clusters and their members with evidence
- Each element's **centrality** — how many other elements depend on it
- Elements in **multiple clusters** — does the element have the same semantics in both? If not,
  that's accidental coupling.
- State **never reset** — unbounded accumulation

Start from the import graph — which files import the most other files in the package? Walk dependencies from the highest-fanout files into their dependents.

## Measuring cohesion

For every field on major types, classify its lifetime. Discover the categories — don't assume them.

Trace each element: where created, where consumed, where destroyed.

**Reset coverage** — for every element with a finite lifetime, verify the reset/rebuild path handles
it. Implicit preservation (not mentioned in the reset path) is a gap.

**Record:**
- Each element's lifetime with evidence
- Containers holding elements from **different lifetimes**
- Elements serving **two roles** at different timescales

## Measuring cognitive load

**Working memory** — at every decision point (if/switch/select), count the concepts that could
affect the outcome.

Counting rules:
- Each local variable: 1 (dead-but-in-scope variables count — readers don't do liveness analysis)
- Each struct field access: 1
- Each distinct map key pattern: 1
- Cross-references to values computed earlier count
- Variables from enclosing loop bodies count at every inner decision point

**Path count** — distinct execution paths through each unit. Many paths AND high working memory
means multiplicative complexity.

## Ranking

Centrality dominates. Between elements at similar centrality, severity breaks the tie.

Within a tier, severity ordering:
1. Lifetime gaps on reachable paths — latent bugs
2. Highest working memory × path count — hardest to modify
3. Mixed-lifetime containers — produce lifetime gaps over time
4. Accidental multi-cluster membership — unnecessary coupling

## Fixing

### Type splits

Split types by lifetime when the measurements show mixed lifetimes. The split is step 1. Step 2 is
narrowing function signatures to accept only the sub-type they need, not the parent. A split without
signature narrowing is documentation, not decoupling.

### Cognitive load extraction

Extract high-count sections into functions with explicit parameter lists. The parameter list IS the
working memory — if it exceeds 7, the extraction reveals genuine coupling. Consider whether the
function is doing work at the wrong level of abstraction.

### Package extraction

When a cluster has a measurably narrow boundary (few types cross it), extract it to a sub-package.

**Import cycles** are engineering problems, not walls. When an extraction creates a bidirectional
dependency, break the cycle by extracting shared types into a common package. This is standard Go
practice — the shared package holds the types both sides need, neither side imports the other.

**Shared receiver problems** are engineering problems, not walls. When a type is the method receiver
across files that should be in different packages, introduce an interface at the consumer side. The
consumer declares what it needs; the producer satisfies it. The interface should be as narrow as the
consumer's actual usage — not the full method set.

**Feature interaction complexity** is the one genuinely irreducible case. When two features compose
and the combined variable count cannot be reduced without removing a feature, the complexity is
inherent to the system's contract. Measure how many variables serve the interaction vs the base
mechanism. Report both numbers. The fix is separating the features into independent units where
possible, not collapsing the variable count.

### One fix per invocation

Make it, compile, test. If the fix requires introducing a shared types package or an interface, that
is the fix for this invocation — do it in one change, not spread across multiple invocations.

## Procedure

### 1. Pick a scope

First invocation: the highest-coupling package.
Subsequent invocations: the most central element from previous findings.

### 2. Measure

Run all three measurements on the scope. Record with file:line evidence. Note centrality.

### 3. Rank

Pick the worst finding by centrality tier then severity.

### 4. Fix

One fix per invocation. Restructure, compile, test.

### 5. Verify

- Code compiles and tests pass
- Coupling is reduced (fewer cross-file write sites, narrower signatures, or extracted packages)
- Cognitive load at affected decision points is lower
- No new violations introduced

### 6. Audit names

For every element touched: does the name predict the behavior?

### 7. Stop

Clean when the worst remaining finding doesn't have a fix that reduces it without introducing new
violations. Report irreducible feature interaction complexity — this is the system's inherent cost.

## What this skill does NOT do

- **Add abstractions speculatively.** Every new type, interface, or package must be justified by
  the measurements. The measurements must show the new structure reduces coupling, improves
  cohesion, or lowers cognitive load.
- **Optimize.** Note performance implications but don't optimize.
- **Minimize.** Low coupling and high cohesion at core elements matters more than fewer elements.
