// Copyright 2025 The Kube Resource Orchestrator Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package graph

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlrtcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/controller/backoff"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	krotruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/schemawatcher"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

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
	Client                  client.Client
	Compiler                Compiler
	Registry                *registry.Registry
	Executor                executor.Interface
	Router                  *watchrouter.Router
	SchemaWatcher           *schemawatcher.SchemaWatcher
	MaxConcurrentReconciles int
	MaxCollectionSize       int

	// Impersonation, when set, resolves a per-Graph executor that applies the
	// Graph's resources while impersonating a ServiceAccount in the Graph's
	// namespace. When nil, the Graph's resources are applied with the kro
	// controller identity (the base Executor).
	Impersonation *impersonationCache

	// RequireImpersonation makes the Graph path fail closed: when true, a Graph
	// reconcile MUST have a working impersonation path (Impersonation and its
	// executor factory wired). If it is not, the Graph is refused rather than
	// silently applied under the kro controller's own (broad) identity — a
	// wiring regression fails safe instead of escalating. Production sets this
	// true; unit tests that construct a Reconciler with only an Executor leave
	// it false to keep the base-executor fallback (see executorFor).
	RequireImpersonation bool

	// ControllerServiceAccount is the impersonation username of kro's OWN
	// ServiceAccount ("system:serviceaccount:<ns>:<name>"), used to refuse a
	// Graph that would impersonate the controller's own (privileged) identity
	// — otherwise `create graphs` in the controller's namespace escalates to
	// whatever the controller SA can do. Empty when not wired (unit tests /
	// impersonation disabled), in which case the guard is a no-op. Any OTHER
	// privileged SA reachable in a namespace is the operator's RBAC concern.
	ControllerServiceAccount string

	// backoff tracks per-Graph consecutive not-ready attempts so the soft
	// ErrNotReady requeue delay grows (capped) instead of polling a
	// never-resolving reference once per second forever. Lazily initialized
	// via backoffOnce so a directly-constructed Reconciler (tests) works too.
	backoff     *backoff.Tracker
	backoffOnce sync.Once
}

// ensureBackoff lazily initializes the per-Graph requeue backoff tracker.
// Safe to call from multiple reconcile workers.
func (r *Reconciler) ensureBackoff() {
	r.backoffOnce.Do(func() {
		if r.backoff == nil {
			r.backoff = backoff.New(backoff.Base)
		}
	})
}

// Reconcile is the main reconcile loop for Graph objects.
//
// Order: deletion handling first, then ensure the finalizer is set, run the
// reconcile body, and finally publish status (with a single
// retry-on-conflict patch). Each path writes its condition via the typed
// ConditionsMarker — never touches Status.Conditions directly.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.ensureBackoff()
	logger := log.FromContext(ctx).WithValues("graph", req.NamespacedName)

	var g expv1alpha1.Graph
	if err := r.Client.Get(ctx, req.NamespacedName, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get graph: %w", err)
	}

	if !g.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, logger, req, &g)
	}

	if err := r.setManaged(ctx, &g); err != nil {
		return ctrl.Result{}, err
	}

	reconcileErr := r.reconcileGraph(ctx, &g)

	// Keep the status-write error separate from the apply error: a soft
	// ErrNotReady is a benign requeue, but a failed status write must never be
	// swallowed (the ErrNotReady classification below is done on the apply
	// error only, so a stale reported state can't hide behind not-ready).
	statusErr := r.updateStatus(ctx, &g)

	// A not-ready signal is a soft requeue: the spec compiled, apply
	// succeeded, the cluster just hasn't converged on readyWhen yet (or a
	// referenced field isn't visible). Return nil error so controller-runtime
	// doesn't apply its own error backoff, but ask for a timed requeue with a
	// capped exponential delay so a never-resolving reference (e.g. a typo)
	// decays to a slow poll instead of a 1/sec hammer.
	if errors.Is(reconcileErr, executor.ErrNotReady) {
		// Apply is a soft requeue — but never discard a status-write failure.
		if statusErr != nil {
			r.backoff.Reset(req.NamespacedName)
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: r.backoff.Next(req.NamespacedName)}, nil
	}
	// Any other outcome (clean converge or a hard error that will be retried
	// with controller-runtime backoff) ends the not-ready streak, so a fixed
	// typo returns to fast requeues on its next stall.
	r.backoff.Reset(req.NamespacedName)
	return ctrl.Result{}, errors.Join(reconcileErr, statusErr)
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
// writeAheadIntent persists the pre-apply intent (managed resources + patch
// contributions) BEFORE any cluster write, and returns the managed-resource
// intent for reuse by the caller's failure branches.
//
// Teardown runs entirely from g.Status.ManagedResources, so a status write lost
// between Apply (which creates children / mutates patch targets) and the
// post-apply persist would orphan those children or leave a contributed field
// with no release-ledger entry. Persisting the union of previous + intended
// identities first guarantees teardown a superset even across a crash in that
// window (mirrors the instance ApplySet union-never-shrinks path). Intent is
// best-effort and UID-free; keyOf dedups post-apply entries. The contribution
// intent's FieldManager is derived from the same executor.PatchFieldManager the
// executor applies under, so a write-ahead entry correlates exactly with the
// contribution Release later looks for (a stale/ghost entry is a tolerated
// no-op: Release GETs the target first and treats absent as already-released).
// Each side only writes when its union grows, so a steady state does not rewrite
// status every cycle.
func (r *Reconciler) writeAheadIntent(
	ctx context.Context,
	g *expv1alpha1.Graph,
	rt *krotruntime.Runtime,
	previous []expv1alpha1.ManagedResource,
	priorContribs []executor.Contribution,
) ([]expv1alpha1.ManagedResource, error) {
	intent := unionManagedResources(previous, intendedManagedResources(rt))
	if len(intent) > len(previous) {
		g.Status.ManagedResources = intent
		if err := r.persistManagedResources(ctx, g); err != nil {
			return nil, fmt.Errorf("write-ahead managed-resource intent: %w", err)
		}
	}

	intentContribs := UnionContributions(priorContribs, intendedContributions(rt))
	if len(intentContribs) > len(priorContribs) {
		if err := r.persistContributions(ctx, g, intentContribs); err != nil {
			return nil, fmt.Errorf("write-ahead patch-contribution intent: %w", err)
		}
	}
	return intent, nil
}

// reconcileDelete tears down a deleting Graph entirely from its persisted
// tracking record (no compile/resolve), so a Graph whose spec was edited
// (rename, forEach shrunk, node dropped) still gets every resource it ever
// applied removed. The executor is resolved from the identity the Graph ACTUALLY
// applied under (persisted in status), not the current spec.serviceAccountName:
// editing that field between apply and delete would run teardown as an identity
// that can no longer see the resources, orphaning children and wedging the
// finalizer. Falls back to the current spec when no applied identity was
// recorded (never applied, or a pre-field kro).
func (r *Reconciler) reconcileDelete(ctx context.Context, logger logr.Logger, req ctrl.Request, g *expv1alpha1.Graph) (ctrl.Result, error) {
	logger.V(1).Info("graph is deleting")
	ex, err := r.teardownExecutorFor(g)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve impersonated executor: %w", err)
	}
	// Surface WHY the finalizer is blocked (instead of returning with the last,
	// possibly healthy, status intact): record a delete-failure condition,
	// persist it best-effort, then requeue.
	teardownFailed := func(cause error, wrap, logMsg string) (ctrl.Result, error) {
		NewConditionsMarkerFor(g).ResourcesDeleteFailed(cause.Error())
		if serr := r.updateStatus(ctx, g); serr != nil {
			logger.Error(serr, logMsg)
		}
		return ctrl.Result{}, fmt.Errorf("%s: %w", wrap, cause)
	}

	if len(g.Status.ManagedResources) > 0 {
		if err := ex.Delete(ctx, g.Status.ManagedResources); err != nil {
			return teardownFailed(err, "executor delete", "failed to persist teardown delete-failure condition")
		}
	}
	// Release every recorded patch contribution so the field managers relinquish
	// their fields. Targets survive — patches never own them.
	if contribs := fromAPIContributions(g.Status.Contributions); len(contribs) > 0 {
		if err := ex.Release(ctx, contribs); err != nil {
			return teardownFailed(err, "executor release", "failed to persist teardown release-failure condition")
		}
	}
	r.Registry.Delete(req.NamespacedName)
	r.backoff.Reset(req.NamespacedName)
	if r.Router != nil {
		r.Router.RemoveGraph(req.NamespacedName)
	}
	if r.SchemaWatcher != nil {
		r.SchemaWatcher.RemoveGraph(req.NamespacedName)
	}
	if err := r.setUnmanaged(ctx, g); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) reconcileGraph(ctx context.Context, g *expv1alpha1.Graph) error {
	marker := NewConditionsMarkerFor(g)
	key := client.ObjectKeyFromObject(g)

	// Refuse a Graph that would impersonate the controller's OWN ServiceAccount.
	// Because the SA is always resolved in the Graph's own namespace, this can
	// only match a Graph in the controller's namespace naming the controller SA
	// (or its default) — which would let `create graphs` there escalate to the
	// controller's privileges. Reject before compile/apply and never requeue as
	// an error: this is a permanent config problem the author must fix.
	if r.impersonatesControllerSelf(g) {
		marker.GraphInvalid(fmt.Sprintf(
			"refusing to apply: Graph would impersonate the kro controller's own ServiceAccount (%q); "+
				"choose a different serviceAccountName or move the Graph out of the controller's namespace",
			r.ControllerServiceAccount))
		return nil
	}

	// Fail closed: when impersonation is required (production) but not wired,
	// refuse to apply rather than falling back to the kro controller's own
	// (broad) identity. This turns a wiring regression into a visible, non-
	// escalating failure. Never requeue as an error: it is a permanent
	// controller misconfiguration an operator must fix.
	if r.RequireImpersonation && (r.Impersonation == nil || r.Impersonation.newExec == nil) {
		marker.ResourcesApplyFailed(
			"refusing to apply: impersonation is required but not configured; " +
				"the Graph controller must be wired with an impersonation path")
		return nil
	}

	// Declare this Graph's schema dependencies from the parsed spec before
	// compilation. This ensures that even if Compile fails (e.g. because a
	// referenced Kind's CRD does not exist yet), the SchemaWatcher tracks
	// the dependency and re-enqueues the Graph when that CRD appears.
	schemaSub := r.schemaSubFor(key)
	trackSchemaDependencies(schemaSub, g.Spec.Nodes)
	schemaSub.Done(true)

	prog, cached, err := r.Registry.Compile(key, g, r.Compiler.Compile)
	if err != nil {
		marker.GraphInvalid(err.Error())
		return err
	}
	if cached {
		log.FromContext(ctx).V(1).Info("compile cache hit", "nodes", len(prog.Nodes))
	}
	marker.GraphCompiled(len(prog.Nodes))

	var rtOpts []krotruntime.Option
	if r.MaxCollectionSize > 0 {
		rtOpts = append(rtOpts, krotruntime.WithMaxCollectionSize(r.MaxCollectionSize))
	}
	rt := krotruntime.New(prog, g, rtOpts...)
	watcher := r.watcherFor(key)
	previous := g.Status.ManagedResources
	priorContribs := fromAPIContributions(g.Status.Contributions)

	// Resolve the executor bound to this Graph's impersonated identity. All
	// resource writes (apply, prune, release) for this Graph go through it so
	// they share one identity confined to the Graph's namespace.
	ex, err := r.executorFor(g)
	if err != nil {
		marker.ResourcesApplyFailed(err.Error())
		return fmt.Errorf("resolve impersonated executor: %w", err)
	}

	// Write-ahead the pre-apply intent (managed resources + patch contributions)
	// BEFORE any cluster write, so a crash between Apply and the post-apply
	// persist still leaves teardown a superset to work from. Returns the
	// managed-resource intent, reused below when a hard/prune failure must keep
	// the inventory from shrinking below the pre-apply superset.
	intent, err := r.writeAheadIntent(ctx, g, rt, previous, priorContribs)
	if err != nil {
		return err
	}

	result, applyErr := ex.Apply(ctx, rt, watcher)

	// Commit on full success or soft ErrNotReady — the executor walks
	// every reachable node even when some are not ready, so the watch
	// set is authoritative either way. Abort only on hard errors that
	// interrupted the walk before downstream watches could register.
	switch {
	case applyErr == nil:
		watcher.Done(true)
		marker.ResourcesConverged()
	case errors.Is(applyErr, executor.ErrNotReady):
		watcher.Done(true)
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
		marker.ResourcesApplyFailed(applyErr.Error())
	}

	// Record the identity that applied under, but ONLY when the apply reached
	// the cluster (clean or soft not-ready) — never on a hard failure, which
	// must preserve the last-good identity so teardown can still see resources a
	// prior identity applied. Empty when impersonation is inactive (the base
	// controller identity), in which case teardown falls back to the spec.
	reachedCluster := applyErr == nil || errors.Is(applyErr, executor.ErrNotReady)
	if reachedCluster {
		if user := r.appliedIdentity(g); user != "" {
			g.Status.AppliedServiceAccount = user
		}
	}

	// Diff previous vs the new applied set. Entries whose NodeID is
	// in result.Unresolved are kept (we don't know their identities
	// this cycle, so don't prune them). Everything else missing from
	// Applied is a prune candidate: node dropped from spec, forEach
	// shrunk, includeWhen flipped, or rename.
	newSet, pruneCandidates := diffManagedResources(previous, result)

	// Prune retired resources whenever the executor walk was not interrupted by
	// a HARD error. diffManagedResources already keeps any entry whose owning
	// node is Unresolved this cycle (in newSet), so pruneCandidates are exactly
	// the resources whose owning node is genuinely gone or resolved — safe to
	// delete even while OTHER nodes are still soft not-ready. Previously all
	// pruning was gated on a fully clean apply, so a single never-ready node
	// vetoed pruning of every unrelated retired node (this is the Graph-path twin
	// of the instance ownedUnresolved/pruneGate narrowing).
	hardErr := applyErr != nil && !errors.Is(applyErr, executor.ErrNotReady)
	// The full pre-apply superset (previous + applied + intent), reused wherever
	// the inventory must not shrink below what the write-ahead already advertised.
	superset := func() []expv1alpha1.ManagedResource {
		return unionManagedResources(unionManagedResources(previous, result.Applied), intent)
	}
	if !hardErr {
		if len(pruneCandidates) > 0 {
			if err := ex.Delete(ctx, pruneCandidates); err != nil {
				// A retired resource could not be deleted (e.g. the impersonated
				// SA lacks delete RBAC). The Graph has NOT converged; surface it
				// and keep the union so teardown still sees the un-deleted entry.
				// (On a soft not-ready cycle this overrides the not-ready marker
				// set above — an un-prunable orphan is the more actionable signal.)
				log.FromContext(ctx).Error(err, "prune failed; keeping union in status")
				marker.ResourcesPruneFailed(err.Error())
				g.Status.ManagedResources = superset()
				return fmt.Errorf("prune: %w", err)
			}
		}
		if applyErr == nil {
			// Fully clean: status is exactly the applied set.
			g.Status.ManagedResources = newSet
		} else {
			// Soft not-ready: the safe prune candidates were just deleted, so
			// they are correctly absent now. Keep applied + still-unresolved-kept
			// entries (newSet) and fold in THIS cycle's write-ahead intent — NOT
			// the previous-based union, which would re-introduce the just-pruned
			// entries — so a crash after Apply still leaves teardown a superset.
			// A removed-from-spec or includeWhen-false node is absent from rt's
			// intent, so it is not re-introduced.
			g.Status.ManagedResources = unionManagedResources(newSet, intendedManagedResources(rt))
		}
	} else {
		// Hard failure — the walk's node outcomes are uncertain, so keep the full
		// union so a future reconcile can still prune or restore. Fold intent back
		// in so the terminal updateStatus cannot shrink the server inventory below
		// the pre-apply superset and re-open the orphan window; UID-free intent
		// dedups against Applied via keyOf.
		g.Status.ManagedResources = superset()
	}

	// Release contributions whose patch node was removed or whose target
	// changed is done on the CLEAN-apply path only (below), symmetric with
	// prune: on a soft/hard error we cannot tell a genuinely-removed patch node
	// apart from one that is merely data-pending this cycle (its contribution
	// is simply absent from result.Contributions either way, and Contribution
	// carries no NodeID to correlate with result.Unresolved), so releasing here
	// would drop fields a still-wanted patch node set — a transient flap. The
	// field-manager-identity-change deadlock this used to guard is now fixed at
	// the source in the executor (contributeApply force-reclaims a same-Graph
	// stale patch identity), so a re-keyed patch node resolves and re-appears in
	// result.Contributions rather than needing a release to break the wedge.

	if applyErr != nil {
		// Soft or hard failure — keep the union so a future reconcile can
		// release contributions we couldn't observe cleanly this cycle.
		if err := r.persistContributions(ctx, g, UnionContributions(priorContribs, result.Contributions)); err != nil {
			return errors.Join(fmt.Errorf("apply: %w", applyErr), err)
		}
		if !errors.Is(applyErr, executor.ErrNotReady) {
			log.FromContext(ctx).Error(applyErr, "executor apply failed")
		}
		return fmt.Errorf("apply: %w", applyErr)
	}

	// Clean apply: release contributions whose patch node was removed or
	// whose target changed, then persist the current inventory. Release runs
	// before persist so a release failure keeps the prior inventory for the
	// next reconcile.
	if released := DiffContributions(priorContribs, result.Contributions); len(released) > 0 {
		if err := ex.Release(ctx, released); err != nil {
			// Apply was clean, so ResourcesConverged was set True above — but a
			// retired patch node's contribution is still on its target with no
			// release inventory, so the Graph has NOT converged. Flip the
			// condition to surface the failure in status instead of reporting
			// Ready=True with the error only in the log (symmetric with the
			// prune-failure branch).
			marker.ResourcesReleaseFailed(err.Error())
			if perr := r.persistContributions(ctx, g, UnionContributions(priorContribs, result.Contributions)); perr != nil {
				return errors.Join(fmt.Errorf("release contributions: %w", err), perr)
			}
			return fmt.Errorf("release contributions: %w", err)
		}
	}
	if err := r.persistContributions(ctx, g, result.Contributions); err != nil {
		return err
	}
	return nil
}

// persistContributions writes the patch-contribution release inventory onto
// the Graph's STATUS subresource, patching only when it changed. Mirrors
// persistManagedResources: it flushes ONLY g.Status.Contributions (retry on
// conflict), never gates on Generation, and keeps the in-memory Graph in sync
// with what was persisted. Persisting on status (not a metadata annotation)
// makes the inventory RBAC-separable — a principal with only spec/metadata
// edit rights cannot forge it.
func (r *Reconciler) persistContributions(
	ctx context.Context,
	g *expv1alpha1.Graph,
	contribs []executor.Contribution,
) error {
	desired := toAPIContributions(contribs)
	if equality.Semantic.DeepEqual(g.Status.Contributions, desired) {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &expv1alpha1.Graph{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
			return fmt.Errorf("refetch graph: %w", err)
		}
		if equality.Semantic.DeepEqual(current.Status.Contributions, desired) {
			// Keep the in-memory Graph consistent with the server.
			g.Status.Contributions = desired
			return nil
		}
		dc := current.DeepCopy()
		dc.Status.Contributions = desired
		if err := r.Client.Status().Patch(ctx, dc, client.MergeFrom(current)); err != nil {
			return err
		}
		g.Status.Contributions = desired
		return nil
	})
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

type typeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// trackSchemaDependencies extracts GroupKind dependencies and dynamic-GVK flags
// directly from the declared GraphSpec nodes and registers them with the
// schema subscription.
func trackSchemaDependencies(sub schemawatcher.Subscription, nodes []expv1alpha1.Node) {
	// trackRaw parses a node's raw manifest for its apiVersion/kind and tracks
	// the GVK dependency. kind names the payload ("patch"/"template") for logging.
	trackRaw := func(nodeID, kind string, raw []byte) {
		if len(raw) == 0 {
			return
		}
		var tm typeMeta
		if err := yaml.Unmarshal(raw, &tm); err != nil {
			log.Log.V(1).Info("failed to parse "+kind+" in trackSchemaDependencies",
				"nodeID", nodeID, "error", err)
			return
		}
		extractAndTrackGVK(sub, nodeID, tm.APIVersion, tm.Kind)
	}
	for i := range nodes {
		n := &nodes[i]
		switch {
		case n.Def != nil:
			// Def nodes define local values without cluster schemas.
		case n.Graph != nil:
			if len(n.Graph.Raw) == 0 {
				continue
			}
			var subSpec expv1alpha1.GraphSpec
			if err := yaml.Unmarshal(n.Graph.Raw, &subSpec); err != nil {
				log.Log.V(1).Info("failed to parse subgraph in trackSchemaDependencies",
					"nodeID", n.ID, "error", err)
				continue
			}
			trackSchemaDependencies(sub, subSpec.Nodes)
		case n.Patch != nil:
			trackRaw(n.ID, "patch", n.Patch.Raw)
		case n.Ref != nil:
			extractAndTrackGVK(sub, n.ID, n.Ref.APIVersion, n.Ref.Kind)
		case n.Template != nil:
			trackRaw(n.ID, "template", n.Template.Raw)
		}
	}
}

func extractAndTrackGVK(sub schemawatcher.Subscription, nodeID, apiVersion, kind string) {
	if strings.Contains(apiVersion, "${") || strings.Contains(kind, "${") {
		sub.TrackDynamic()
	}

	if kind == "" || strings.Contains(kind, "${") {
		return
	}

	// apiVersion has no expressions: parse standard GroupVersion.
	if !strings.Contains(apiVersion, "${") {
		if apiVersion == "" {
			return
		}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			log.Log.V(1).Info("failed to parse apiVersion in trackSchemaDependencies",
				"nodeID", nodeID, "apiVersion", apiVersion, "kind", kind, "error", err)
			return
		}
		sub.Track(schema.GroupKind{Group: gv.Group, Kind: kind})
		return
	}

	// apiVersion is dynamic, but the group segment might be static: e.g.
	// "apps/${version}" or "example.com/${version}".
	if group, _, ok := strings.Cut(apiVersion, "/"); ok && group != "" && !strings.Contains(group, "${") {
		sub.Track(schema.GroupKind{Group: group, Kind: kind})
	}
}

// setManaged ensures the Graph carries the finalizer. Uses a strategic patch
// against the freshly-fetched object with retry-on-conflict.
func (r *Reconciler) setManaged(ctx context.Context, g *expv1alpha1.Graph) error {
	if metadata.HasGraphFinalizer(g) {
		return nil
	}
	log.FromContext(ctx).V(1).Info("setting graph as managed")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &expv1alpha1.Graph{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
			return err
		}
		if metadata.HasGraphFinalizer(current) {
			return nil
		}
		dc := current.DeepCopy()
		metadata.SetGraphFinalizer(dc)
		if err := r.Client.Patch(ctx, dc, client.MergeFrom(current)); err != nil {
			return err
		}
		metadata.SetGraphFinalizer(g)
		return nil
	})
	if err != nil {
		return fmt.Errorf("set managed: %w", err)
	}
	return nil
}

// setUnmanaged drops the Graph finalizer if present so the API server can
// complete deletion. The deletion path removes managed resources before
// calling this.
func (r *Reconciler) setUnmanaged(ctx context.Context, g *expv1alpha1.Graph) error {
	if !metadata.HasGraphFinalizer(g) {
		return nil
	}
	log.FromContext(ctx).V(1).Info("setting graph as unmanaged")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &expv1alpha1.Graph{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if !metadata.HasGraphFinalizer(current) {
			return nil
		}
		dc := current.DeepCopy()
		metadata.RemoveGraphFinalizer(dc)
		return r.Client.Patch(ctx, dc, client.MergeFrom(current))
	})
	if err != nil {
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
		dc := current.DeepCopy()
		if current.Generation != g.Generation {
			logger.V(1).Info("skipping stale status conditions write but preserving managed resources union",
				"observed-generation", g.Generation,
				"current-generation", current.Generation)
			dc.Status.ManagedResources = unionManagedResources(current.Status.ManagedResources, g.Status.ManagedResources)
		} else {
			dc.Status.Conditions = g.Status.Conditions
			dc.Status.ManagedResources = g.Status.ManagedResources
		}
		// Persist the applied identity whenever we have one (never regress a
		// recorded identity to empty), independent of the generation gate: it
		// tracks the identity that applied the resources, which teardown depends
		// on and which is orthogonal to spec generation.
		if g.Status.AppliedServiceAccount != "" {
			dc.Status.AppliedServiceAccount = g.Status.AppliedServiceAccount
		}

		if equality.Semantic.DeepEqual(current.Status, dc.Status) {
			return nil
		}
		logger.V(1).Info("updating graph status",
			"conditions", len(dc.Status.Conditions),
			"managedResources", len(dc.Status.ManagedResources))
		return r.Client.Status().Patch(ctx, dc, client.MergeFrom(current))
	})
}

// impersonatesControllerSelf reports whether g would apply its resources under
// the kro controller's OWN ServiceAccount identity. Guards the escalation where
// a Graph in the controller's namespace names the controller SA (or the
// namespace default resolves to it). A no-op when ControllerServiceAccount is
// unset (impersonation disabled / unit tests).
func (r *Reconciler) impersonatesControllerSelf(g *expv1alpha1.Graph) bool {
	return r.ControllerServiceAccount != "" &&
		serviceAccountUsername(g) == r.ControllerServiceAccount
}

// persistManagedResources flushes ONLY g.Status.ManagedResources (retry on
// conflict), leaving Status.Conditions untouched. The write-ahead vehicle for
// the pre-apply intent: unlike updateStatus it never gates on Generation and
// unions with the live inventory, so the write can only grow the tracked set.
func (r *Reconciler) persistManagedResources(ctx context.Context, g *expv1alpha1.Graph) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &expv1alpha1.Graph{}
		if err := r.Client.Get(ctx, client.ObjectKeyFromObject(g), current); err != nil {
			return fmt.Errorf("refetch graph: %w", err)
		}
		dc := current.DeepCopy()
		union := unionManagedResources(current.Status.ManagedResources, g.Status.ManagedResources)
		dc.Status.ManagedResources = union
		// Keep the in-memory Graph consistent with what we persisted so the
		// post-apply diff/prune below reasons about the same superset.
		g.Status.ManagedResources = union
		if equality.Semantic.DeepEqual(current.Status, dc.Status) {
			return nil
		}
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
	if r.MaxConcurrentReconciles > 0 {
		b = b.WithOptions(ctrlrtcontroller.Options{
			MaxConcurrentReconciles: r.MaxConcurrentReconciles,
		})
	}
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

// ResourcesPruneFailed marks ResourcesConverged=False with reason
// "PruneFailed" when the apply was clean but a retired resource could not
// be deleted (e.g. the impersonated ServiceAccount lacks delete RBAC on the
// target). The resource the spec no longer wants still lives in the cluster,
// so the Graph has not converged — surfacing this keeps Ready from reporting
// True while an orphan lingers.
func (m *ConditionsMarker) ResourcesPruneFailed(msg string) {
	m.cs.SetFalse(ResourcesConverged, "PruneFailed", msg)
}

// ResourcesReleaseFailed marks ResourcesConverged=False with reason
// "ReleaseFailed" when the apply was clean but a retired patch node's
// contribution could not be released from its target (e.g. the impersonated
// ServiceAccount lacks patch RBAC, or the target's CRD was removed). The
// contributed field lingers on the target with no release inventory, so the
// Graph has not converged — surfacing this keeps Ready from reporting True
// while a stale contributed field remains.
func (m *ConditionsMarker) ResourcesReleaseFailed(msg string) {
	m.cs.SetFalse(ResourcesConverged, "ReleaseFailed", msg)
}

// ResourcesDeleteFailed marks ResourcesConverged=False with reason
// "DeleteFailed" when teardown (finalizer path) could not delete a managed
// resource or release a patch contribution — e.g. the persisted applying
// identity lacks delete/patch RBAC. Without this, a wedged finalizer would
// keep reporting the Graph's last (possibly healthy) status with no signal as
// to why deletion is blocked; recording it lets an operator see the cause.
func (m *ConditionsMarker) ResourcesDeleteFailed(msg string) {
	m.cs.SetFalse(ResourcesConverged, "DeleteFailed", msg)
}
