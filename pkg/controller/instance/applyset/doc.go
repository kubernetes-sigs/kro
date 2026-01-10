// Package applyset implements KEP-3659 ApplySet for kro's instance controller.
//
// # Responsibilities
//
// ApplySet: server-side apply resources, label them for membership, return metadata.
// Controller: convert metadata to labels/annotations, call Prune separately, decide
// which metadata to write based on prune outcome.
//
// # Workflow
//
// 1. Project() computes union metadata (batch + parent annotations). Error = requeue.
// 2. Controller patches parent with union metadata (before apply/prune)
// 3. Apply() runs SSA and returns batch-only metadata
// 4. Prune() deletes orphans using scope from Project().PruneScope()
// 5. If prune completes, controller shrinks parent annotations to batch metadata
//
// ApplySet is stateless - all methods are pure functions of their inputs.
//
// # Metadata Semantics
//
// Apply() returns batch-only metadata: just the GKs and namespaces of this batch.
// Project() returns union metadata: batch + parent annotations (the "memory").
// Controller chooses which to write based on prune outcome.
//
// # Prune Safety
//
// IMPORTANT: Prune only runs if ALL applies succeeded. If any apply failed,
// the failed resource's UID won't be in keepUIDs, so pruning would delete
// a potentially working resource. Use Errors() to check safety.
//
// # SkipApply
//
// Resources with SkipApply=true are NOT applied (no SSA call). They are also
// excluded from the current reconcile's GK/namespace set. However, if the
// resource was previously applied, its GK remains in the parent annotation
// ("memory"), and prune will find and delete it. This is how includeWhen=false
// resources get cleaned up: they were applied before, now they're skipped,
// and the parent annotation provides prune scope from prior reconciles.
//
// # ApplySet ID
//
// Computed from parent GKNN: applyset-<base64(sha256(name.namespace.kind.group))>-v1
// Stable across reconciles, unique per instance.
//
// # Parent annotations (controller applies from Metadata)
//
//   - applyset.kubernetes.io/tooling: identifies kro as the manager
//   - applyset.kubernetes.io/contains-group-kinds: GKs of managed resources (shrinks/expands dynamically)
//   - applyset.kubernetes.io/additional-namespaces: namespaces where managed resources live
//
// # Child labels (applied during SSA)
//
//   - applyset.kubernetes.io/part-of: links child to parent's ApplySet ID
package applyset
