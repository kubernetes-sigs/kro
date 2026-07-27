// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/apis"
	"sigs.k8s.io/krocodile/pkg/compiler"
	"sigs.k8s.io/krocodile/pkg/executor"
	"sigs.k8s.io/krocodile/pkg/metadata"
	"sigs.k8s.io/krocodile/pkg/registry"
	krotruntime "sigs.k8s.io/krocodile/pkg/runtime"
	"sigs.k8s.io/krocodile/pkg/schemawatcher"
	"sigs.k8s.io/krocodile/pkg/watchrouter"
)

// notReadyRequeueAfter is how long we wait before re-checking readiness
// when an executor surfaces ErrNotReady. Short by default so converging
// graphs settle quickly; if it becomes an API-server load problem we'll
// add per-Graph backoff in status.
const notReadyRequeueAfter = 1 * time.Second

// Compiler is the narrow surface the reconciler needs from a Compiler. Kept
// as an interface so tests can substitute a fake without spinning up a real
// cluster.
type Compiler interface {
	Compile(*expv1alpha1.Graph) (*compiler.Program, error)
}

// Reconciler reconciles Graph objects. Compilation is mediated through a
// Registry so Graphs only recompile when their normalized spec hash changes.
// The Executor is consulted on every reconcile to converge the cluster
// toward the compiled Program's desired state. The Router hands out
// per-Graph Watchers so the executor can register interest in each
// resolved resource — drift events flow back through Router.Source()
// into the controller-runtime work queue.
//
// The SchemaWatcher (if wired) tracks which CRD GroupKinds each Graph
// depends on. On a CRD content change the watcher invalidates the
// compile cache for affected Graphs and enqueues them — the next
// reconcile recompiles against the fresh schema.
type Reconciler struct {
	Client        client.Client
	Compiler      Compiler
	Registry      *registry.Registry
	Executor      executor.Interface
	Router        *watchrouter.Router
	SchemaWatcher *schemawatcher.SchemaWatcher
}

// Reconcile is the main reconcile loop for Graph objects.
//
// The shape mirrors kro's RGD controller: deletion handling first, then
// ensure the finalizer is set, run the reconcile body, and finally publish
// status (with a single retry-on-conflict patch). Each path writes its
// condition via the typed ConditionsMarker — never touches Status.Conditions
// directly.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("graph", req.NamespacedName)

	var g expv1alpha1.Graph
	if err := r.Client.Get(ctx, req.NamespacedName, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get Graph: %w", err)
	}

	if !g.DeletionTimestamp.IsZero() {
		logger.V(1).Info("graph is deleting")
		// Delete operates entirely from the persisted tracking record.
		// No compile, no resolve — a Graph whose spec was edited
		// (rename, forEach shrunk, node dropped) still gets every
		// resource it ever applied removed.
		if len(g.Status.ManagedResources) > 0 {
			if err := r.Executor.Delete(ctx, g.Status.ManagedResources); err != nil {
				return ctrl.Result{}, fmt.Errorf("executor delete: %w", err)
			}
		}
		r.Registry.Delete(req.NamespacedName)
		if r.Router != nil {
			r.Router.RemoveGraph(req.NamespacedName)
		}
		if r.SchemaWatcher != nil {
			r.SchemaWatcher.RemoveGraph(req.NamespacedName)
		}
		if err := r.setUnmanaged(ctx, &g); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.setManaged(ctx, &g); err != nil {
		return ctrl.Result{}, err
	}

	reconcileErr := r.reconcileGraph(ctx, &g)

	if err := r.updateStatus(ctx, &g); err != nil {
		reconcileErr = errors.Join(reconcileErr, err)
	}
	// A not-ready signal is a soft requeue: the spec compiled, apply
	// succeeded, the cluster just hasn't converged on readyWhen yet.
	// Return nil error so controller-runtime doesn't apply error backoff,
	// but ask for a timed requeue.
	if errors.Is(reconcileErr, executor.ErrNotReady) {
		return ctrl.Result{RequeueAfter: notReadyRequeueAfter}, nil
	}
	return ctrl.Result{}, reconcileErr
}

// reconcileGraph runs the actual reconciliation body. Compilation goes
// through the Registry so identical specs across reconciles share one
// compiled Program. Conditions are written via the ConditionsMarker; status
// is flushed by the caller via updateStatus.
//
// If a Router is wired in, we open a per-Graph Watcher around
// Apply so each resolved resource registers a watch. On success commit
// the new watch set (clearing any nodes that were removed in this
// revision); on error abort, keeping the previously committed set
// authoritative so drift detection survives transient failures.
func (r *Reconciler) reconcileGraph(ctx context.Context, g *expv1alpha1.Graph) error {
	marker := NewConditionsMarkerFor(g)
	key := client.ObjectKeyFromObject(g)

	prog, cached, err := r.Registry.Compile(key, g, r.Compiler.Compile)
	if err != nil {
		marker.GraphInvalid(err.Error())
		log.FromContext(ctx).Error(err, "compile failed")
		return err
	}
	if cached {
		log.FromContext(ctx).V(1).Info("compile cache hit", "nodes", len(prog.Nodes))
	}
	marker.GraphCompiled(len(prog.Nodes))

	// Declare this Graph's schema dependencies. The schema watcher
	// uses these to know which Graphs to re-enqueue when a CRD's
	// content changes. Done(commit) happens at the bottom of the
	// reconcile body, after Apply completes — the same two-cycle
	// pattern the watch coordinator uses.
	schemaSub := r.schemaSubFor(key)
	for _, gk := range prog.RequiredGroupKinds {
		schemaSub.Track(gk)
	}
	if prog.HasDynamicGVK {
		schemaSub.TrackDynamic()
	}

	rt := krotruntime.New(prog, g)
	watcher := r.watcherFor(key)
	previous := g.Status.ManagedResources

	result, applyErr := r.Executor.Apply(ctx, rt, watcher)

	// Commit on full success or soft ErrNotReady — the executor walks
	// every reachable node even when some are not ready, so the watch
	// set is authoritative either way. Abort only on hard errors that
	// interrupted the walk before downstream watches could register.
	// The schema subscription follows the same logic: commit on
	// success/soft, abort on hard, so a hard error doesn't shrink the
	// committed schema dependency set authoritatively.
	switch {
	case applyErr == nil:
		watcher.Done(true)
		schemaSub.Done(true)
		marker.ResourcesConverged()
	case errors.Is(applyErr, executor.ErrNotReady):
		watcher.Done(true)
		schemaSub.Done(true)
		// Distinguish the two not-ready flavors so principals can tell
		// "apply succeeded, cluster still settling" from "upstream data
		// isn't visible yet, can't even resolve dependents."
		if errors.Is(applyErr, krotruntime.ErrDataPending) {
			marker.ResourcesDataPending(applyErr.Error())
		} else {
			marker.ResourcesNotReady(applyErr.Error())
		}
	default:
		watcher.Done(false)
		schemaSub.Done(false)
		marker.ResourcesApplyFailed(applyErr.Error())
	}

	// Diff previous vs the new applied set. Entries whose NodeID is
	// in result.Unresolved are kept (we don't know their identities
	// this cycle, so don't prune them). Everything else missing from
	// Applied is a prune candidate: node dropped from spec, forEach
	// shrunk, includeWhen flipped, or rename.
	newSet, pruneCandidates := diffManagedResources(previous, result)

	if applyErr == nil {
		// Only prune on a fully clean Apply. If we have any soft errors
		// (Unresolved is non-empty) or hard errors, keep the union of
		// previous and applied to protect resources we couldn't observe
		// this cycle.
		if len(pruneCandidates) > 0 {
			if err := r.Executor.Delete(ctx, pruneCandidates); err != nil {
				// Prune failure isn't catastrophic — next reconcile
				// retries with the same diff. But we shouldn't shrink
				// status to newSet if some prune candidates are still
				// in the cluster, so keep the union.
				log.FromContext(ctx).Error(err, "prune failed; keeping union in status")
				g.Status.ManagedResources = unionManagedResources(previous, result.Applied)
				return fmt.Errorf("prune: %w", err)
			}
		}
		g.Status.ManagedResources = newSet
	} else {
		// Soft or hard failure — keep the union so a future reconcile
		// can still find the resources to prune or restore.
		g.Status.ManagedResources = unionManagedResources(previous, result.Applied)
	}

	if applyErr != nil {
		log.FromContext(ctx).Error(applyErr, "executor apply failed")
		return fmt.Errorf("apply: %w", applyErr)
	}
	return nil
}

// watcherFor returns a per-Graph Watcher when a Router is
// wired, or a NoopWatcher otherwise. The Noop fallback keeps the
// reconciler usable in unit tests and dry-run contexts.
func (r *Reconciler) watcherFor(key client.ObjectKey) watchrouter.Watcher {
	if r.Router == nil {
		return watchrouter.NoopWatcher{}
	}
	return r.Router.ForGraph(key)
}

// schemaSubFor returns a per-Graph schema Subscription when a watcher
// is wired, or a no-op subscription otherwise.
func (r *Reconciler) schemaSubFor(key client.ObjectKey) schemawatcher.Subscription {
	if r.SchemaWatcher == nil {
		return noopSchemaSubscription{}
	}
	return r.SchemaWatcher.ForGraph(key)
}

// noopSchemaSubscription is the inert fallback used when the
// reconciler has no SchemaWatcher (unit tests, CLI / dry-run).
type noopSchemaSubscription struct{}

func (noopSchemaSubscription) Track(schema.GroupKind) {}
func (noopSchemaSubscription) TrackDynamic()          {}
func (noopSchemaSubscription) Done(bool)              {}

// setManaged ensures the Graph carries the finalizer. Uses a strategic patch
// against the freshly-fetched object so concurrent metadata writes don't
// trigger a 409.
func (r *Reconciler) setManaged(ctx context.Context, g *expv1alpha1.Graph) error {
	if metadata.HasGraphFinalizer(g) {
		return nil
	}
	log.FromContext(ctx).V(1).Info("setting graph as managed")
	dc := g.DeepCopy()
	metadata.SetGraphFinalizer(dc)
	if err := r.Client.Patch(ctx, dc, client.MergeFrom(g)); err != nil {
		return fmt.Errorf("set managed: %w", err)
	}
	// Reflect the change in the in-memory copy so later patches see it.
	metadata.SetGraphFinalizer(g)
	return nil
}

// setUnmanaged drops the Graph finalizer if present so the API server can
// complete deletion. Today there is no in-cluster state owned by the Graph;
// when a runtime is wired in, additional cleanup will run before this.
func (r *Reconciler) setUnmanaged(ctx context.Context, g *expv1alpha1.Graph) error {
	if !metadata.HasGraphFinalizer(g) {
		return nil
	}
	log.FromContext(ctx).V(1).Info("setting graph as unmanaged")
	dc := g.DeepCopy()
	metadata.RemoveGraphFinalizer(dc)
	if err := r.Client.Patch(ctx, dc, client.MergeFrom(g)); err != nil {
		return fmt.Errorf("set unmanaged: %w", err)
	}
	return nil
}

// updateStatus flushes Status fields onto the API server with a retry on
// conflict. Conditions are only published if g.Generation still matches
// the live object's generation — a stale generation means the spec
// changed mid-reconcile and our condition values (computed against the
// old spec) would be misleading. In that case we skip the write and let
// the next reconcile re-evaluate against the fresh spec.
//
// The DeepEqual short-circuit avoids no-op writes, which keeps the
// generation churn down and prevents needless re-reconciles.
func (r *Reconciler) updateStatus(ctx context.Context, g *expv1alpha1.Graph) error {
	logger := log.FromContext(ctx)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &expv1alpha1.Graph{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
			return fmt.Errorf("refetch graph: %w", err)
		}
		// Skip if the spec moved out from under us. The next reconcile
		// will run against current.Spec and publish fresh conditions.
		if current.Generation != g.Generation {
			logger.V(1).Info("skipping stale status write",
				"observed-generation", g.Generation,
				"current-generation", current.Generation)
			return nil
		}
		dc := current.DeepCopy()
		dc.Status.Conditions = g.Status.Conditions
		dc.Status.ManagedResources = g.Status.ManagedResources

		if equality.Semantic.DeepEqual(current.Status, dc.Status) {
			return nil
		}
		logger.V(1).Info("updating graph status",
			"conditions", len(dc.Status.Conditions),
			"managedResources", len(dc.Status.ManagedResources))
		return r.Client.Status().Patch(ctx, dc, client.MergeFrom(current))
	})
}

// SetupWithManager registers the reconciler with the manager. When a
// Router is wired, its event channel is added as a raw source so drift
// events on watched resources flow into the same work queue as Graph
// spec updates. Same applies to the SchemaWatcher — CRD content
// changes feed Graph re-reconciles through a second raw source.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).For(&expv1alpha1.Graph{})
	if r.Router != nil {
		b = b.WatchesRawSource(r.Router.Source())
	}
	if r.SchemaWatcher != nil {
		b = b.WatchesRawSource(r.SchemaWatcher.Source())
	}
	return b.Complete(r)
}

// --- Condition vocabulary ---------------------------------------------------

// Condition type names exposed by the Graph reconciler. Ready is the
// root condition; ConditionSet rolls it up from the listed dependents.
const (
	Ready              = string(expv1alpha1.GraphConditionTypeReady)
	GraphAccepted      = string(expv1alpha1.GraphConditionTypeAccepted)
	ResourcesConverged = "ResourcesConverged"
)

// graphConditionTypes registers Accepted and ResourcesConverged as
// dependents of Ready. Accepted reports compilation; ResourcesConverged
// reports the executor's terminal apply state — Ready stays Unknown
// until both flip True, False if either is False.
var graphConditionTypes = apis.NewReadyConditions(GraphAccepted, ResourcesConverged)

// ConditionsMarker is the typed surface for writing Graph conditions. Each
// method touches exactly one condition; the root Ready condition is
// recomputed by the underlying ConditionSet.
type ConditionsMarker struct {
	cs apis.ConditionSet
}

// NewConditionsMarkerFor binds a ConditionsMarker to a specific Graph. The
// returned marker mutates g.Status.Conditions in place.
func NewConditionsMarkerFor(g *expv1alpha1.Graph) *ConditionsMarker {
	return &ConditionsMarker{cs: graphConditionTypes.For(g)}
}

// GraphCompiled marks Accepted=True with reason "Compiled" and a message
// summarising the compiled node count.
func (m *ConditionsMarker) GraphCompiled(nodes int) {
	m.cs.SetTrueWithReason(GraphAccepted, "Compiled", fmt.Sprintf("compiled %d nodes", nodes))
}

// GraphInvalid marks Accepted=False with reason "InvalidGraph" and the
// supplied compile error as the message.
func (m *ConditionsMarker) GraphInvalid(msg string) {
	m.cs.SetFalse(GraphAccepted, "InvalidGraph", msg)
}

// ResourcesConverged marks ResourcesConverged=True with reason "Applied"
// after every node has applied and reported ready.
func (m *ConditionsMarker) ResourcesConverged() {
	m.cs.SetTrueWithReason(ResourcesConverged, "Applied", "all nodes applied and ready")
}

// ResourcesNotReady marks ResourcesConverged=False with reason
// "WaitingForReadiness" — the apply succeeded but readyWhen
// expressions evaluated false.
func (m *ConditionsMarker) ResourcesNotReady(msg string) {
	m.cs.SetFalse(ResourcesConverged, "WaitingForReadiness", msg)
}

// ResourcesDataPending marks ResourcesConverged=False with reason
// "DataPending" — a node's CEL expression referenced data the cluster
// hasn't surfaced yet (typically a status field). Distinct from
// WaitingForReadiness so operators can tell a stuck readyWhen from a
// resolution gap.
func (m *ConditionsMarker) ResourcesDataPending(msg string) {
	m.cs.SetFalse(ResourcesConverged, "DataPending", msg)
}

// ResourcesApplyFailed marks ResourcesConverged=False with reason
// "ApplyFailed" when the executor returned a hard error.
func (m *ConditionsMarker) ResourcesApplyFailed(msg string) {
	m.cs.SetFalse(ResourcesConverged, "ApplyFailed", msg)
}
