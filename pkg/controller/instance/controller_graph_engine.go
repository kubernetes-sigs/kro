// Copyright 2025 The Kubernetes Authors.
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

// controller_graph_engine.go — reconciles a non-deleting instance through the
// Graph engine.

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	controllergraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/dynamiccontroller"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	geruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// reconcileViaGraphEngine reconciles a non-deleting instance. Deletion uses the
// ApplySet/finalizer path instead.
//
// Steps:
//  1. Resolve the RGD spec from the revision registry.
//  2. Build a per-reconcile Runtime via rgdadapter.BuildRuntimeForInstance.
//  3. Apply all Graph nodes via the executor.Simple, wired to the instance's
//     dynamiccontroller.InstanceWatcher (via instanceWatcherBridge).
//  4. The synthesized status patch node writes the author status FIELDS onto
//     the instance during executor.Apply; the controller writes conditions +
//     .status.state via persistGraphEngineStatus.
//  5. Project author conditions via rgdadapter.ProjectInstanceConditions and
//     merge them into the conditions written by the controller.
//
// Error policy: hard errors propagate to controller-runtime for
// requeue-with-backoff; ErrNotReady from the executor is a soft signal that the
// call succeeds but the instance stays in InProgress.
func (c *Controller) reconcileViaGraphEngine(
	ctx context.Context,
	inst *unstructured.Unstructured,
	dcWatcher dynamiccontroller.InstanceWatcher,
) error {
	log := c.log.WithValues(
		"namespace", inst.GetNamespace(),
		"name", inst.GetName(),
		"path", "graph-engine",
	)

	gvrStr := c.gvr.String()

	// Resolve the RGD spec from the revision registry.
	latest, ok := c.graphResolver.GetLatestRevision()
	if !ok {
		metrics.InstanceGraphResolutionPendingTotal.WithLabelValues(gvrStr).Inc()
		mark := NewConditionsMarkerFor(inst)
		mark.GraphResolutionFailed("graph not resolved: latest issued revision not found")
		if err := c.updateConditionsStatus(ctx, inst); err != nil {
			log.V(1).Info("graph-engine: failed to update conditions status", "error", err)
		}
		return c.delayedRequeue(fmt.Errorf("graph-engine: latest issued revision not available"))
	}
	if latest.State == revisions.RevisionStateFailed {
		metrics.InstanceGraphResolutionFailuresTotal.WithLabelValues(gvrStr, "revision_failed").Inc()
		mark := NewConditionsMarkerFor(inst)
		mark.GraphResolutionFailed("graph not resolved: latest issued revision %d failed", latest.Revision)
		if err := c.updateConditionsStatus(ctx, inst); err != nil {
			log.V(1).Info("graph-engine: failed to update conditions status", "error", err)
		}
		return requeue.None(fmt.Errorf("graph-engine: latest issued revision %d failed", latest.Revision))
	}
	if latest.State != revisions.RevisionStateActive {
		metrics.InstanceGraphResolutionPendingTotal.WithLabelValues(gvrStr).Inc()
		mark := NewConditionsMarkerFor(inst)
		mark.GraphResolutionFailed("graph not resolved: latest issued revision %d is not active (state=%s)", latest.Revision, latest.State)
		if err := c.updateConditionsStatus(ctx, inst); err != nil {
			log.V(1).Info("graph-engine: failed to update conditions status", "error", err)
		}
		return c.delayedRequeue(fmt.Errorf("graph-engine: latest issued revision %d is not active (state=%s)", latest.Revision, latest.State))
	}
	if latest.RGDSpec == nil {
		metrics.InstanceGraphResolutionPendingTotal.WithLabelValues(gvrStr).Inc()
		// The revision doesn't carry an RGDSpec, so there is no engine to run
		// yet — requeue until the graphrevision controller reprocesses the
		// revision and populates it.
		log.V(1).Info("graph-engine: RGDSpec not available in revision; requeuing until the graphrevision controller repopulates it")
		return c.requeueUntilRGDSpecPopulated(ctx, inst)
	}

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: latest.OwnerKey,
		},
		Spec: *latest.RGDSpec,
	}

	// Guard: the compiler must be wired via WithGraphEngineCompiler.
	if c.graphEngineCompiler == nil {
		return fmt.Errorf("graph-engine: compiler not wired (WithGraphEngineCompiler not called); this is a programming error")
	}

	// Stamp the kro finalizer and management labels on the instance.
	if patched, err := c.stampInstanceMetadata(ctx, inst); err != nil {
		return err
	} else if patched != nil {
		inst.Object = patched.Object
	}

	// Snapshot wire status BEFORE the marker writes built-in defaults, and
	// build the condition marker on the (possibly rebound) instance.
	wireStatus := captureWireStatus(inst)
	mark := NewConditionsMarkerFor(inst)
	mark.InstanceManaged()

	// Read prior contributions off the instance annotations.
	priorContribs, err := controllergraph.ReadContributions(inst)
	if err != nil {
		log.Error(err, "graph-engine: malformed patch-contribution inventory")
		mark.ResourcesNotReady("malformed patch-contribution inventory: %v", err)
		if updateErr := c.updateConditionsStatus(ctx, inst); updateErr != nil {
			log.V(1).Info("graph-engine: failed to update conditions status", "error", updateErr)
		}
		return c.delayedRequeue(fmt.Errorf("read patch contributions: %w", err))
	}

	// Build a per-reconcile Runtime.
	var rtOpts []geruntime.Option
	if c.reconcileConfig.MaxCollectionSize > 0 {
		rtOpts = append(rtOpts, geruntime.WithMaxCollectionSize(c.reconcileConfig.MaxCollectionSize))
	}
	rt, _, err := rgdadapter.BuildRuntimeForInstanceCached(rgd, inst, c.graphEngineCompiler, c.programCache, rtOpts...)
	if err != nil {
		metrics.InstanceGraphResolutionFailuresTotal.WithLabelValues(gvrStr, "build_failed").Inc()
		log.Error(err, "graph-engine: BuildRuntimeForInstance failed")
		// Mark the instance so the operator can see the build failure.
		mark.GraphResolutionFailed("graph-engine build failed: %v", err)
		if updateErr := c.updateConditionsStatus(ctx, inst); updateErr != nil {
			log.V(1).Info("graph-engine: failed to update conditions status", "error", updateErr)
		}
		return err
	}
	metrics.InstanceGraphResolutionSuccessTotal.WithLabelValues(gvrStr).Inc()
	mark.GraphResolved()

	// 1. Pre-apply ApplySet inventory projection & grow.
	// Persist the superset inventory to the parent instance BEFORE applying resources
	// to the cluster. This eliminates the crash window where resources exist in the cluster
	// but the parent has no inventory tracking them.
	supersetMeta, applier, preErr := c.preApplyApplySetInventory(ctx, log, inst, rt)
	if preErr != nil {
		return preErr
	}

	// Apply through the executor (SSA + watches).
	// Build a per-reconcile child labeler: instance labels + applyset part-of
	// + struct-level KRO-meta labels are composed inside ApplyWithLabeler.
	instanceLabeler := metadata.NewInstanceLabeler(inst, c.namespaced)
	nodeLabeler := metadata.NewNodeLabeler()
	applysetPartOf := applyset.ID(inst)
	extraLabel := func(obj *unstructured.Unstructured) {
		instanceLabeler.ApplyLabels(obj)
		// app.kubernetes.io/managed-by=kro.
		nodeLabeler.ApplyLabels(obj)
		l := obj.GetLabels()
		if l == nil {
			l = map[string]string{}
		}
		l[applyset.ApplysetPartOfLabel] = applysetPartOf
		obj.SetLabels(l)
	}
	bridge := &instanceWatcherBridge{w: dcWatcher}
	applyResult, applyErr := c.graphEngineExecutor.ApplyWithLabeler(ctx, rt, bridge, extraLabel)

	valErr := validateAppliedIdentities(applyResult.Applied)

	hardErr := false
	switch {
	case valErr != nil:
		hardErr = true
		applyErr = valErr
		mark.ResourcesNotReady("duplicate resource in graph: %v", valErr)
	case applyErr == nil:
		mark.ResourcesReady()
	case isResourceDeleting(applyErr):
		// A managed resource is terminating (has a deletionTimestamp). Hold
		// ResourcesReady=False with reason "ResourceDeleting" and a message
		// naming the resource, keep the instance InProgress, and let the node
		// gate its dependents (via GateReadiness) so the downstream resource is
		// not created until deletion completes. Checked before the generic
		// ErrNotReady branch because ResourceDeletingError satisfies both
		// sentinels.
		var delErr *executor.ResourceDeletingError
		if errors.As(applyErr, &delErr) {
			mark.ResourcesDeleting("%v", delErr)
		} else {
			mark.ResourcesDeleting("%v", applyErr)
		}
	case errors.Is(applyErr, executor.ErrNotReady):
		// Soft: a node is waiting on data/readiness. State stays InProgress;
		// child watch events (and the requeue below) drive the next cycle.
		mark.ResourcesNotReady("waiting for unresolved resource: %v", applyErr)
	default:
		hardErr = true
		mark.ResourcesNotReady("resource reconciliation failed: %v", applyErr)
	}

	// 2. Post-apply ApplySet prune & exact-batch shrink.
	// Only when the desired set is fully resolved and apply had no hard error do we prune
	// resources that left the desired set, then shrink the inventory to the exact current set.
	fullyResolved := pruneGate(hardErr, applyResult.Unresolved)
	if invErr := c.reconcileApplySetInventory(ctx, log, inst, applier, applyResult.Applied, supersetMeta, fullyResolved); invErr != nil {
		log.Error(invErr, "graph-engine: ApplySet inventory/prune failed")
		if applyErr == nil {
			applyErr = invErr
		}
	}

	// Reconcile patch contributions: release pruned contributions and persist inventory.
	if contribErr := c.reconcilePatchContributions(ctx, log, inst, priorContribs, applyResult.Contributions, applyErr); contribErr != nil {
		if applyErr == nil {
			applyErr = contribErr
		}
	}

	// Persist the controller-owned status surface (built-in + author conditions
	// and .status.state), skip-write guarded by statusesMatch. The author
	// status FIELDS are written by the synthesized status patch node during
	// executor.Apply above, under its own field manager — the controller no
	// longer projects them here, so the two writers stay on disjoint fields.
	degraded := hardErr

	if err := c.persistGraphEngineStatus(ctx, inst, wireStatus, rt, rgd, degraded); err != nil {
		log.Error(err, "graph-engine: status persist failed")
		return err
	}

	// All apply outcomes (success, soft-not-ready, or retryable apply error)
	// have their conditions and status persisted above. Requeue on any apply error
	// with the configured interval so the instance retries without counting as
	// a reconcile-level infrastructural error.
	if applyErr != nil {
		return c.delayedRequeue(applyErr)
	}
	return nil
}

func (c *Controller) delayedRequeue(err error) error {
	if c.reconcileConfig.DefaultRequeueDuration == 0 {
		return requeue.None(err)
	}
	return requeue.NeededAfter(err, c.reconcileConfig.DefaultRequeueDuration)
}

// requeueUntilRGDSpecPopulated handles a revision entry with no RGDSpec. There
// is no engine to run yet, so it requeues until the graphrevision controller
// reprocesses the revision and populates RGDSpec, at which point the instance
// reconciles on the next cycle.
// pruneGate decides whether the ApplySet prune step may run this cycle. Pruning
// is permitted only when apply had no HARD error (soft ErrNotReady/data-pending
// is fine) AND every node resolved — an unresolved node means some still-wanted
// member may merely be absent from Applied this cycle, so pruning would delete
// it. This combines both signals in one place so the wiring is unit-testable
// (removing either clause is caught by TestPruneGate).
func pruneGate(hardErr bool, unresolved []string) bool {
	return !hardErr && len(unresolved) == 0
}

func (c *Controller) requeueUntilRGDSpecPopulated(ctx context.Context, inst *unstructured.Unstructured) error {
	mark := NewConditionsMarkerFor(inst)
	mark.GraphResolutionFailed("graph-engine: revision entry has no RGDSpec")
	if err := c.updateConditionsStatus(ctx, inst); err != nil {
		c.log.V(1).Info("graph-engine: failed to update conditions status", "error", err)
	}
	return c.delayedRequeue(
		fmt.Errorf("graph-engine: revision entry has no RGDSpec; waiting for the graphrevision controller to repopulate it"),
	)
}

// reconcileApplySetInventory writes the ApplySet inventory metadata on the
// instance and prunes resources that left the desired set.
//
// The inventory is written as the UNION of the newly-applied group-kinds/
// namespaces and the prior parent "memory" (the values already recorded in the
// instance's ApplySet annotations).  Mirroring applyset.Project, this union
// guarantees the inventory never shrinks on a not-ready/degraded cycle — which
// is what keeps the deletion path from finding zero managed resources and
// orphaning children when a dependent is transiently withheld.
//
// Pruning is gated on fullyResolved (!hardErr && no Unresolved nodes):
// we must never prune while anything is unresolved, or we would delete
// still-wanted members that were merely omitted from Applied this cycle.  Only
// after a conflict-free prune that actually removed orphans do we shrink the
// inventory to the exact current set.
// reconcileApplySetInventory prunes resources that left the desired set and shrinks the
// inventory annotation to the exact batch of applied resources.
func (c *Controller) reconcileApplySetInventory(
	ctx context.Context,
	log logr.Logger,
	inst *unstructured.Unstructured,
	applier *applyset.ApplySet,
	applied []v1alpha1.ManagedResource,
	supersetMeta applyset.Metadata,
	fullyResolved bool,
) error {
	if valErr := validateAppliedIdentities(applied); valErr != nil {
		return valErr
	}

	if applier == nil {
		applier = applyset.New(applyset.Config{
			Client:          c.client.Dynamic(),
			RESTMapper:      c.client.RESTMapper(),
			Log:             log,
			ParentNamespace: inst.GetNamespace(),
		}, inst)
	}

	// If pre-apply union was empty, compute superset now from the applied set and parent memory.
	if len(supersetMeta.GroupKinds) == 0 {
		batchMeta := applySetMetadataFromApplied(inst, applied)
		var unionErr error
		supersetMeta, unionErr = applier.Union(batchMeta)
		if unionErr != nil {
			log.V(1).Info("graph-engine: applyset union failed", "error", unionErr)
			return fmt.Errorf("applyset union: %w", unionErr)
		}
	}

	// Pruning is gated on fullyResolved (!hardErr && no Unresolved nodes):
	// we must never prune while anything is unresolved, or we would delete
	// still-wanted members that were merely omitted from Applied this cycle.
	if !fullyResolved {
		return nil
	}

	_, conflictFree, err := c.pruneGraphEngineOrphans(ctx, log, applier, applied, supersetMeta)
	if err != nil {
		return err
	}

	// Align the inventory to the exact applied batch when fully resolved.
	if fullyResolved && conflictFree {
		batchMeta := applySetMetadataFromApplied(inst, applied)
		if err := c.patchInstanceApplySetMetadata(ctx, inst, batchMeta); err != nil {
			log.V(1).Info("graph-engine: failed to align inventory after apply/prune", "error", err)
			return fmt.Errorf("align inventory after apply/prune: %w", err)
		}
	}
	return nil
}

// validateAppliedIdentities checks that no two distinct nodeIDs in the applied set
// target the exact same Kubernetes object identity (GVK, namespace, and name).
func validateAppliedIdentities(applied []v1alpha1.ManagedResource) error {
	seen := make(map[string]string, len(applied))
	var conflicts []string
	for _, r := range applied {
		key := fmt.Sprintf("%s/%s/%s/%s", r.APIVersion, r.Kind, r.Namespace, r.Name)
		if existingNodeID, ok := seen[key]; ok && existingNodeID != r.NodeID {
			conflicts = append(conflicts, fmt.Sprintf("node %q conflicts with node %q for %s", r.NodeID, existingNodeID, key))
		}
		seen[key] = r.NodeID
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s", applyset.ErrDuplicateResource, strings.Join(conflicts, ", "))
	}
	return nil
}

// preApplyApplySetInventory projects candidate metadata from the runtime in memory,
// computes the superset inventory, and persists it to the parent instance before
// any cluster SSA writes. This eliminates the crash window where resources exist
// in the cluster but the parent has no inventory tracking them.
func (c *Controller) preApplyApplySetInventory(
	ctx context.Context,
	log logr.Logger,
	inst *unstructured.Unstructured,
	rt *geruntime.Runtime,
) (applyset.Metadata, *applyset.ApplySet, error) {
	applier := applyset.New(applyset.Config{
		Client:          c.client.Dynamic(),
		RESTMapper:      c.client.RESTMapper(),
		Log:             log,
		ParentNamespace: inst.GetNamespace(),
	}, inst)

	candidateMeta := c.candidateMetadata(rt, inst)
	supersetMeta, projErr := applier.Union(candidateMeta)
	if projErr != nil {
		log.Error(projErr, "graph-engine: pre-apply applyset union failed")
		return supersetMeta, applier, c.delayedRequeue(fmt.Errorf("pre-apply applyset union failed: %w", projErr))
	}

	if err := c.patchInstanceApplySetMetadata(ctx, inst, supersetMeta); err != nil {
		log.Error(err, "graph-engine: failed to patch pre-apply superset inventory")
		return supersetMeta, applier, c.delayedRequeue(fmt.Errorf("patch pre-apply superset inventory: %w", err))
	}
	return supersetMeta, applier, nil
}

// candidateMetadata collects candidate ApplySet metadata (GroupKinds and
// AdditionalNamespaces) expected to be managed this cycle from the runtime's template
// nodes, without constructing artificial Kubernetes objects.
func (c *Controller) candidateMetadata(rt *geruntime.Runtime, inst *unstructured.Unstructured) applyset.Metadata {
	meta := applyset.Metadata{
		ID:                   applyset.ID(inst),
		Tooling:              applyset.ToolingID(),
		GroupKinds:           sets.New[schema.GroupKind](),
		AdditionalNamespaces: sets.New[string](),
	}
	parentNS := inst.GetNamespace()

	// Seed Def nodes (such as the synthesized `schema` instance node) into the runtime
	// scope first so that includeWhen expressions and template expressions referencing
	// `schema.spec...` can resolve in memory.
	for _, n := range rt.Nodes() {
		if n.Kind() == compiler.NodeKindDef {
			if desired, err := n.Resolve(); err == nil && len(desired) > 0 {
				n.SetObserved(desired, desired)
				if n.IsCollection() {
					list := make([]any, 0, len(desired))
					for _, obj := range desired {
						list = append(list, obj.Object)
					}
					rt.Set(n.ID(), list)
				} else {
					rt.Set(n.ID(), desired[0].Object)
				}
				if rt.Program() != nil {
					if sc, ok := rt.Program().NodeSchemas[n.ID()]; ok && sc != nil {
						rt.Scope()[n.ID()] = celunstructured.UnstructuredToVal(desired[0].Object, &openapi.Schema{Schema: sc})
					}
				}
			}
		}
	}

	for _, n := range rt.Nodes() {
		if n.Kind() != compiler.NodeKindTemplate {
			continue
		}

		// If the node resolves cleanly in memory, extract its rendered GroupKinds
		// and additional namespaces directly from the rendered objects.
		if desired, err := n.Resolve(); err == nil {
			for _, obj := range desired {
				gvk := obj.GroupVersionKind()
				if gvk.Kind != "" {
					meta.GroupKinds.Insert(gvk.GroupKind())
				}
				ns := obj.GetNamespace()
				if ns == "" && n.Namespaced() {
					ns = parentNS
				}
				if ns != "" && ns != parentNS {
					meta.AdditionalNamespaces.Insert(ns)
				}
			}
			continue
		}

		// Fallback for nodes that cannot resolve yet (e.g. data pending on an
		// unready upstream resource): extract static GroupKind from the compiled spec.
		if specObj := n.Spec().Object; specObj != nil {
			gvk := specObj.GroupVersionKind()
			if gvk.Kind != "" {
				meta.GroupKinds.Insert(gvk.GroupKind())
			}
		}
	}
	return meta
}

// applySetMetadataFromApplied builds ApplySet inventory metadata from only the
// resources applied this cycle (the "batch" set), excluding the parent
// namespace from AdditionalNamespaces per KEP-3659.
func applySetMetadataFromApplied(inst *unstructured.Unstructured, applied []v1alpha1.ManagedResource) applyset.Metadata {
	meta := applyset.Metadata{
		ID:                   applyset.ID(inst),
		Tooling:              applyset.ToolingID(),
		GroupKinds:           sets.New[schema.GroupKind](),
		AdditionalNamespaces: sets.New[string](),
	}
	parentNS := inst.GetNamespace()
	for _, r := range applied {
		gv, err := schema.ParseGroupVersion(r.APIVersion)
		if err != nil {
			continue
		}
		meta.GroupKinds.Insert(schema.GroupKind{Group: gv.Group, Kind: r.Kind})
		// AdditionalNamespaces excludes the parent namespace per KEP-3659.
		if r.Namespace != "" && r.Namespace != parentNS {
			meta.AdditionalNamespaces.Insert(r.Namespace)
		}
	}
	return meta
}

// pruneGraphEngineOrphans discovers applyset members not in the applied set and
// deletes them in reverse apply-order (dependents before dependencies).  It
// returns whether any orphan was actually removed and whether the prune was
// free of UID conflicts.  NotFound and UID-conflict deletes are tolerated by
// DeleteOrphan; a conflict leaves the object in place and is reported so the
// caller keeps the superset inventory for a later retry.
func (c *Controller) pruneGraphEngineOrphans(
	ctx context.Context,
	log logr.Logger,
	applier *applyset.ApplySet,
	applied []v1alpha1.ManagedResource,
	supersetMeta applyset.Metadata,
) (pruned bool, conflictFree bool, err error) {
	keepUIDs := sets.New[types.UID]()
	for _, r := range applied {
		if r.UID != "" {
			keepUIDs.Insert(types.UID(r.UID))
		}
	}

	candidates, err := applier.ListOrphans(ctx, applyset.PruneOptions{
		KeepUIDs: keepUIDs,
		Scope:    supersetMeta.PruneScope(),
	})
	if err != nil {
		return false, false, fmt.Errorf("list orphans: %w", err)
	}
	if len(candidates) == 0 {
		return false, true, nil
	}

	// Delete dependents before dependencies: sort by the persisted apply-order
	// annotation descending.  Unmapped/invalid orders sort first (treated as
	// the highest wave) so nodes removed from the graph entirely are deleted
	// first.
	sort.SliceStable(candidates, func(i, j int) bool {
		return orphanApplyOrder(candidates[i]) > orphanApplyOrder(candidates[j])
	})

	conflicts := 0
	for _, candidate := range candidates {
		res, derr := applier.DeleteOrphan(ctx, candidate)
		if derr != nil {
			return pruned, false, fmt.Errorf("delete orphan: %w", derr)
		}
		if res.Pruned != nil {
			pruned = true
		}
		if res.Conflict {
			conflicts++
		}
	}
	if conflicts > 0 {
		log.V(1).Info("graph-engine: prune skipped resources due to UID conflicts; keeping superset inventory", "conflicts", conflicts)
		return pruned, false, nil
	}
	return pruned, true, nil
}

// orphanApplyOrder reads the persisted reverse-topological apply-order wave for
// an orphan candidate.  Missing/invalid orders return max int so unmapped
// resources (whose node was removed from the graph) are deleted first.
func orphanApplyOrder(candidate applyset.OrphanCandidate) int {
	raw := candidate.Object.GetAnnotations()[metadata.ApplyOrderAnnotation]
	order, err := strconv.Atoi(raw)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return order
}

// patchInstanceApplySetMetadata writes the supplied ApplySet inventory metadata
// on the instance.  All four KEP-3659 annotations (tooling, contains-group-
// kinds, additional-namespaces, and the inventory hash) plus the parent-id
// label are written together so they stay mutually consistent — writing
// group-kinds without recomputing the hash would fail ValidateParentInventory
// and wedge deletion.
func (c *Controller) patchInstanceApplySetMetadata(ctx context.Context, inst *unstructured.Unstructured, meta applyset.Metadata) error {
	wantLabels := meta.Labels()
	wantAnnotations := meta.Annotations()

	// Fast-path: skip the write when the inventory is already correct.
	if inventoryUpToDate(inst, wantLabels, wantAnnotations) {
		return nil
	}

	patchObj := instanceSSAPatch(inst)
	patchObj.SetLabels(wantLabels)
	patchObj.SetAnnotations(wantAnnotations)

	ri := c.client.Dynamic().Resource(c.gvr)
	var instClient dynamic.ResourceInterface = ri
	if c.namespaced {
		instClient = ri.Namespace(inst.GetNamespace())
	}
	updated, err := instClient.Apply(ctx, inst.GetName(), patchObj, metav1.ApplyOptions{
		FieldManager: applyset.FieldManager + "-parent",
		Force:        true,
	})
	if err != nil {
		return err
	}
	if updated != nil {
		inst.SetLabels(updated.GetLabels())
		inst.SetAnnotations(updated.GetAnnotations())
		inst.SetResourceVersion(updated.GetResourceVersion())
	} else {
		inst.SetLabels(wantLabels)
		anns := inst.GetAnnotations()
		if anns == nil {
			anns = make(map[string]string)
		}
		maps.Copy(anns, wantAnnotations)
		inst.SetAnnotations(anns)
	}
	return nil
}

// reconcilePatchContributions releases contributions that are no longer desired (pruned)
// and persists the updated patch-contribution inventory on the instance.
func (c *Controller) reconcilePatchContributions(
	ctx context.Context,
	log logr.Logger,
	inst *unstructured.Unstructured,
	priorContribs, currentContribs []executor.Contribution,
	applyErr error,
) error {
	if applyErr != nil {
		// Soft or hard failure — keep the union so a future reconcile can
		// release contributions we couldn't observe cleanly this cycle.
		if perr := c.persistContributions(ctx, inst, controllergraph.UnionContributions(priorContribs, currentContribs)); perr != nil {
			log.Error(perr, "graph-engine: failed to persist union patch contributions")
		}
		return applyErr
	}

	// Clean apply: release contributions whose patch node was removed or
	// whose target changed, then persist the current inventory. Release runs
	// before persist so a release failure keeps the prior inventory for the
	// next reconcile.
	if released := controllergraph.DiffContributions(priorContribs, currentContribs); len(released) > 0 {
		if relErr := c.graphEngineExecutor.Release(ctx, released); relErr != nil {
			log.Error(relErr, "graph-engine: release contributions failed")
			if perr := c.persistContributions(ctx, inst, controllergraph.UnionContributions(priorContribs, currentContribs)); perr != nil {
				log.Error(perr, "graph-engine: failed to persist union patch contributions on release error")
			}
			return relErr
		}
	}
	if perr := c.persistContributions(ctx, inst, currentContribs); perr != nil {
		log.Error(perr, "graph-engine: failed to persist patch contributions")
		return perr
	}
	return nil
}

// persistContributions writes the patch-contribution inventory onto the
// instance as an annotation, patching only when the value changed. An empty
// inventory drops the annotation.
func (c *Controller) persistContributions(ctx context.Context, inst *unstructured.Unstructured, contribs []executor.Contribution) error {
	value, err := controllergraph.MarshalContributions(contribs)
	if err != nil {
		return fmt.Errorf("marshal contributions: %w", err)
	}
	if inst.GetAnnotations()[metadata.PatchContributionsAnnotation] == value {
		return nil
	}

	var patchData []byte
	if value == "" {
		patchData, err = json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					metadata.PatchContributionsAnnotation: nil,
				},
			},
		})
	} else {
		patchData, err = json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"annotations": map[string]interface{}{
					metadata.PatchContributionsAnnotation: value,
				},
			},
		})
	}
	if err != nil {
		return fmt.Errorf("marshal patch contributions patch: %w", err)
	}

	ri := c.client.Dynamic().Resource(c.gvr)
	var instClient dynamic.ResourceInterface = ri
	if c.namespaced {
		instClient = ri.Namespace(inst.GetNamespace())
	} else {
		instClient = ri
	}
	updated, err := instClient.Patch(ctx, inst.GetName(), types.MergePatchType, patchData, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("persist contributions: %w", err)
	}
	if updated != nil {
		inst.SetAnnotations(updated.GetAnnotations())
		inst.SetResourceVersion(updated.GetResourceVersion())
	} else {
		anns := inst.GetAnnotations()
		if anns == nil {
			anns = map[string]string{}
		}
		if value == "" {
			delete(anns, metadata.PatchContributionsAnnotation)
		} else {
			anns[metadata.PatchContributionsAnnotation] = value
		}
		inst.SetAnnotations(anns)
	}
	return nil
}

// inventoryUpToDate reports whether inst already carries every supplied
// ApplySet label/annotation with the same value.
func inventoryUpToDate(inst *unstructured.Unstructured, wantLabels, wantAnnotations map[string]string) bool {
	haveLabels := inst.GetLabels()
	for k, v := range wantLabels {
		if haveLabels[k] != v {
			return false
		}
	}
	haveAnnotations := inst.GetAnnotations()
	for k, v := range wantAnnotations {
		if haveAnnotations[k] != v {
			return false
		}
	}
	return true
}

// persistGraphEngineStatus composes the controller-owned status surface for the
// instance (built-in conditions, author conditions when the RGD declares them,
// and .status.state) and persists it through persistConditionsAndState, reusing
// the statusesMatch skip-write and the state-transition metric. The author
// status FIELDS are NOT written here — the synthesized status patch node owns
// them under its own field manager, so the controller's writer and the node's
// writer touch disjoint fields.
//
// degraded forces state=Error regardless of condition readiness.
func (c *Controller) persistGraphEngineStatus(
	ctx context.Context,
	inst *unstructured.Unstructured,
	wireStatus map[string]interface{},
	rt *geruntime.Runtime,
	rgd *v1alpha1.ResourceGraphDefinition,
	degraded bool,
) error {
	previousState, _ := wireStatus["state"].(string)

	// Only conditions and state: author status fields belong to the patch node.
	status := map[string]interface{}{}

	builtins := builtinConditions(inst)
	status["conditions"] = conditionsToInterfaceSlice(builtins)

	cs := condSet.For(&unstructuredWrapper{inst})
	switch {
	case degraded:
		status["state"] = string(v1alpha1.InstanceStateError)
	case cs.IsRootReady():
		status["state"] = string(v1alpha1.InstanceStateActive)
	default:
		status["state"] = string(v1alpha1.InstanceStateInProgress)
	}

	if c.reconcileConfig.HasAuthorConditions {
		authored, incomplete, condErr := rgdadapter.ProjectInstanceConditions(rt, rgd, builtins)
		prev, _ := wireStatus["conditions"].([]interface{})
		previous := decodeConditions(prev)
		stamped := stampAuthorConditions(authored, previous, inst.GetGeneration())
		if incomplete {
			stamped = mergeWithPrevious(stamped, previous)
		}
		status["conditions"] = conditionsToInterfaceSlice(stamped)
		if condErr != nil {
			c.log.Error(condErr, "graph-engine: author conditions degraded; setting state=Error")
			status["state"] = string(v1alpha1.InstanceStateError)
		}
	}

	return c.persistConditionsAndState(ctx, inst, wireStatus, status, previousState)
}

// instanceWatcherBridge adapts a dynamiccontroller.InstanceWatcher to the
// watchrouter.Watcher interface expected by executor.Simple.  The two
// WatchRequest types are structurally equivalent (NodeID, GVR, Name,
// Namespace) — only the package path differs.
type instanceWatcherBridge struct {
	w dynamiccontroller.InstanceWatcher
}

func (b *instanceWatcherBridge) Watch(req watchrouter.WatchRequest) error {
	return b.w.Watch(dynamiccontroller.WatchRequest{
		NodeID:    req.NodeID,
		GVR:       req.GVR,
		Name:      req.Name,
		Namespace: req.Namespace,
		Selector:  req.Selector,
	})
}

func (b *instanceWatcherBridge) Done(commit bool) {
	b.w.Done(commit)
}

// isResourceDeleting reports whether err (an executor apply error) signals a
// managed resource that is currently terminating. It matches both the
// distinguishable sentinel and the typed error the executor wraps.
func isResourceDeleting(err error) bool {
	// (*ResourceDeletingError).Is reports ErrResourceDeleting, so errors.Is
	// matches the typed error anywhere in the chain — no separate errors.As.
	return errors.Is(err, executor.ErrResourceDeleting)
}
