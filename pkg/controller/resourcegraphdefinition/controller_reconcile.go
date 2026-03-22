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

package resourcegraphdefinition

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	instancectrl "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

var errGraphRevisionsNotResolved = errors.New("graph revisions not resolved")

// reconcileResourceGraphDefinition orchestrates the reconciliation of a ResourceGraphDefinition.
//
// See KREP-013 for the full design.
//  1. Reconcile graph revisions to determine whether serving can
//     continue, must wait, or must issue a new revision.
//  2. If issuance is required, build and validate the current graph, create a
//     new GraphRevision, and wait for the GR controller to compile it.
//  3. Once the latest revision is compiled, ensure the CRD and microcontroller.
//  4. Garbage-collect old GraphRevisions per --rgd-max-graph-revisions.
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinition(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, []string, []v1alpha1.ResourceInformation, error) {
	log := ctrl.LoggerFrom(ctx)
	mark := NewConditionsMarkerFor(rgd)

	currentSpecHash, err := graphhash.Spec(rgd.Spec)
	if err != nil {
		mark.ResourceGraphInvalid(err.Error())
		return ctrl.Result{}, nil, nil, err
	}

	graphRevisions, hasTerminating, err := r.listGraphRevisions(ctx, rgd)
	if err != nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("listing graph revisions: %w", err)
	}

	// If no live revisions exist, clear any stale registry entries from a
	// previous RGD with the same name. At this point hasTerminating is false,
	// so all old revisions are fully gone — any remaining registry entries
	// are orphaned leftovers.
	if len(graphRevisions) == 0 && !hasTerminating {
		r.revisionsRegistry.DeleteAll(rgd.Name)
	}

	latestRevisionView, warmed := r.getLatestGraphRevisionView(rgd.Name, graphRevisions)
	if err := r.resolveGraphRevisions(rgd, currentSpecHash, latestRevisionView, hasTerminating, warmed, mark); err != nil {
		if errors.Is(err, errGraphRevisionsNotResolved) {
			return ctrl.Result{RequeueAfter: r.cfg.ProgressRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		}
		return ctrl.Result{}, rgd.Status.TopologicalOrder, rgd.Status.Resources, err
	}

	latestRevision := latestRevisionView.Revision
	noRevision := latestRevision == nil
	// Safe to deref RuntimeEntry: resolveGraphRevisions returns
	// errGraphRevisionsNotResolved when RuntimeEntry is nil, so reaching
	// here with a non-nil revision guarantees RuntimeEntry is set.
	specChanged := !noRevision && latestRevisionView.RuntimeEntry.SpecHash != currentSpecHash
	if noRevision || specChanged {
		reason := "spec changed"
		if noRevision {
			reason = "no existing revision"
		}
		log.V(1).Info("building resource graph definition", "reason", reason)
		// TODO(amine): replace with a lighter validation-only pass that doesn't
		// produce a full compiled graph.
		if _, _, err := r.buildResourceGraphDefinition(ctx, rgd); err != nil {
			mark.ResourceGraphInvalid(err.Error())
			return ctrl.Result{}, nil, nil, err
		}
		mark.ResourceGraphValid()

		revision := max(rgd.Status.LastIssuedRevision, latestRevisionView.RevisionNumber) + 1
		createdGR, createErr := r.createGraphRevision(ctx, rgd, revision, currentSpecHash)
		if createErr != nil {
			mark.ResourceGraphInvalid(createErr.Error())
			return ctrl.Result{}, nil, nil, createErr
		}
		rgd.Status.LastIssuedRevision = createdGR.Spec.Revision
		mark.GraphRevisionsCompiling(createdGR.Spec.Revision)
		return ctrl.Result{RequeueAfter: r.cfg.ProgressRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
	}

	topologicalOrder, resourcesInfo, err := r.ensureServingState(
		ctx,
		rgd,
		latestRevisionView.RuntimeEntry.CompiledGraph,
		mark,
	)

	// Best-effort retention GC: runs inline after serving state is established.
	// Failures are logged and recorded as a metric but do not block the reconcile.
	if gcErr := r.garbageCollectGraphRevisions(ctx, rgd); gcErr != nil {
		graphRevisionGCErrorsTotal.WithLabelValues(rgd.Name).Inc()
		log.Error(gcErr, "failed to garbage collect graph revisions", "name", rgd.Name)
	}

	return ctrl.Result{}, topologicalOrder, resourcesInfo, err
}

// setupMicroController creates a new controller instance with the required configuration
func (r *ResourceGraphDefinitionReconciler) setupMicroController(
	rgd *v1alpha1.ResourceGraphDefinition,
	processedRGD *graph.Graph,
	instanceLabeler metadata.Labeler,
) *instancectrl.Controller {
	gvr := processedRGD.Instance.Meta.GVR
	instanceLogger := r.instanceLogger.WithName(fmt.Sprintf("%s-controller", gvr.Resource)).WithValues(
		"controller", gvr.Resource,
		"controllerGroup", processedRGD.CRD.Spec.Group,
		"controllerKind", processedRGD.CRD.Spec.Names.Kind,
	)

	return instancectrl.NewController(
		instanceLogger,
		instancectrl.ReconcileConfig{
			DefaultRequeueDuration:    r.cfg.InstanceRequeueInterval,
			DeletionGraceTimeDuration: 30 * time.Second,
			DeletionPolicy:            "Delete",
			RGDConfig:                 r.cfg.RGDConfig,
		},
		gvr,
		r.revisionsRegistry.ResolverForRGD(rgd.Name),
		processedRGD.Instance.Meta.Namespaced,
		r.clientSet,
		instanceLabeler,
		r.metadataLabeler,
		r.dynamicController.Coordinator(),
		// recorder keyed by CRD name to uniquely identify the event source
		r.newEventRecorder(fmt.Sprintf("kro/%s-controller", processedRGD.CRD.Name)),
	)
}

// resolveGraphRevisions determines whether the RGD can proceed to serving
// or must wait for revision state to stabilize.
//
// Decision order:
//  1. Terminating revisions exist → wait for GC to settle before making decisions.
//  2. Latest revision not in informer cache → wait for cache warmup.
//  3. No revisions exist → caller issues a fresh revision.
//  4. Latest revision in informer but not in registry → wait for GR controller
//     to process it (avoids issuing a duplicate).
//  5. Hash mismatch → caller revalidates the spec and issues a new revision.
//  6. Latest revision is Active → graph revisions resolved, proceed to serving.
//  7. Latest revision is Pending → wait for GR controller to compile.
//  8. Latest revision is Failed → surface the failure, no requeue.
//
// Returns errGraphRevisionsNotResolved for cases that need a requeue, nil for
// cases where the caller should proceed, and a real error for terminal failures.
func (r *ResourceGraphDefinitionReconciler) resolveGraphRevisions(
	rgd *v1alpha1.ResourceGraphDefinition,
	currentSpecHash string,
	latestRevisionView latestGraphRevisionView,
	hasTerminating bool,
	warmed bool,
	mark *ConditionsMarker,
) error {
	if hasTerminating {
		mark.GraphRevisionsSettling()
		return errGraphRevisionsNotResolved
	}

	if !warmed {
		mark.GraphRevisionsWarmingUp()
		return errGraphRevisionsNotResolved
	}

	latestRevision := latestRevisionView.Revision
	if latestRevision == nil {
		return nil
	}

	rgd.Status.LastIssuedRevision = max(rgd.Status.LastIssuedRevision, latestRevision.Spec.Revision)

	// Revision exists in the informer but not yet in the registry — the GR
	// controller hasn't processed it. Wait rather than re-issuing a duplicate.
	if latestRevisionView.RuntimeEntry == nil {
		mark.GraphRevisionsWarmingUp()
		return errGraphRevisionsNotResolved
	}

	if latestRevisionView.RuntimeEntry.SpecHash != currentSpecHash {
		// Graph revisions are resolved (old revision Active) but spec changed.
		// Caller will validate and issue a new revision.
		mark.GraphRevisionsResolved(latestRevision.Spec.Revision)
		return nil
	}

	switch latestRevisionView.RuntimeEntry.State {
	case revisions.RevisionStateActive:
		mark.GraphRevisionsResolved(latestRevision.Spec.Revision)
		mark.ResourceGraphValid()
		return nil
	case revisions.RevisionStatePending:
		mark.GraphRevisionsAwaitingCompilation(latestRevision.Spec.Revision)
		return errGraphRevisionsNotResolved
	case revisions.RevisionStateFailed:
		failErr := fmt.Errorf("latest graph revision %d failed compilation", latestRevision.Spec.Revision)
		mark.GraphRevisionsUnresolved(failErr.Error())
		mark.ResourceGraphInvalid(failErr.Error())
		return failErr
	default:
		mark.GraphRevisionsAwaitingSettlement(latestRevision.Spec.Revision)
		return errGraphRevisionsNotResolved
	}
}

// buildResourceGraphDefinition compiles the desired graph from the current RGD spec.
func (r *ResourceGraphDefinitionReconciler) buildResourceGraphDefinition(_ context.Context, rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	startTime := time.Now()
	defer func() {
		graphBuildDuration.WithLabelValues(rgd.Name).Observe(time.Since(startTime).Seconds())
		graphBuildTotal.WithLabelValues(rgd.Name).Inc()
	}()

	processedRGD, err := r.rgBuilder.NewResourceGraphDefinition(rgd, r.cfg.RGDConfig)
	if err != nil {
		graphBuildErrorsTotal.WithLabelValues(rgd.Name).Inc()
		return nil, nil, newGraphError(err)
	}

	return processedRGD, resourcesInfoFromGraph(processedRGD), nil
}

// buildResourceInfo creates a ResourceInformation struct from name and dependencies
func buildResourceInfo(name string, deps []string) v1alpha1.ResourceInformation {
	dependencies := make([]v1alpha1.Dependency, 0, len(deps))
	for _, dep := range deps {
		dependencies = append(dependencies, v1alpha1.Dependency{ID: dep})
	}
	return v1alpha1.ResourceInformation{
		ID:           name,
		Dependencies: dependencies,
	}
}

func resourcesInfoFromGraph(processedRGD *graph.Graph) []v1alpha1.ResourceInformation {
	resourcesInfo := make([]v1alpha1.ResourceInformation, 0, len(processedRGD.Nodes))
	for _, name := range processedRGD.TopologicalOrder {
		node := processedRGD.Nodes[name]
		deps := node.Meta.Dependencies
		if len(deps) > 0 {
			resourcesInfo = append(resourcesInfo, buildResourceInfo(name, deps))
		}
	}
	return resourcesInfo
}

// ensureResourceGraphDefinitionCRD ensures the CRD is present and up to date in the cluster.
func (r *ResourceGraphDefinitionReconciler) ensureResourceGraphDefinitionCRD(ctx context.Context, crd *v1.CustomResourceDefinition, allowBreakingChanges bool) error {
	if err := r.crdManager.Ensure(ctx, *crd, allowBreakingChanges); err != nil {
		return newCRDError(err)
	}
	return nil
}

// ensureResourceGraphDefinitionController starts the microcontroller for handling the resources.
// Child/external resource watches are discovered dynamically by the coordinator from
// Watch() calls made by instance reconcilers -- no GVR list needed here.
func (r *ResourceGraphDefinitionReconciler) ensureResourceGraphDefinitionController(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	processedRGD *graph.Graph,
	instanceLabeler metadata.Labeler,
) error {
	controller := r.setupMicroController(rgd, processedRGD, instanceLabeler)

	ctrl.LoggerFrom(ctx).V(1).Info("reconciling resource graph definition micro controller")
	gvr := processedRGD.Instance.Meta.GVR

	err := r.dynamicController.Register(ctx, gvr, controller.Reconcile)
	if err != nil {
		return newMicroControllerError(err)
	}
	return nil
}

func (r *ResourceGraphDefinitionReconciler) ensureServingState(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	processedRGD *graph.Graph,
	mark *ConditionsMarker,
) ([]string, []v1alpha1.ResourceInformation, error) {
	log := ctrl.LoggerFrom(ctx)

	resourcesInfo := resourcesInfoFromGraph(processedRGD)

	rgdLabeler := metadata.NewResourceGraphDefinitionLabeler(rgd)
	instanceLabeler, err := r.metadataLabeler.Merge(rgdLabeler)
	if err != nil {
		mark.FailedLabelerSetup(err.Error())
		return nil, nil, fmt.Errorf("failed to setup labeler: %w", err)
	}

	crd := processedRGD.CRD
	instanceLabeler.ApplyLabels(&crd.ObjectMeta)

	log.V(1).Info("ensuring resource graph definition CRD")
	allowBreakingChanges := rgd.Annotations[v1alpha1.AllowBreakingChangesAnnotation] == "true"
	if err := r.ensureResourceGraphDefinitionCRD(ctx, crd, allowBreakingChanges); err != nil {
		mark.KindUnready(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}
	if crd, err = r.crdManager.Get(ctx, crd.Name); err != nil {
		mark.KindUnready(err.Error())
	} else {
		mark.KindReady(crd.Status.AcceptedNames.Kind)
	}

	if err := r.ensureResourceGraphDefinitionController(ctx, rgd, processedRGD, instanceLabeler); err != nil {
		mark.ControllerFailedToStart(err.Error())
		return processedRGD.TopologicalOrder, resourcesInfo, err
	}
	mark.ControllerRunning()

	return processedRGD.TopologicalOrder, resourcesInfo, nil
}

// Error types for the resourcegraphdefinition controller
type (
	graphError           struct{ err error }
	crdError             struct{ err error }
	microControllerError struct{ err error }
)

// Error interface implementation
func (e *graphError) Error() string           { return e.err.Error() }
func (e *crdError) Error() string             { return e.err.Error() }
func (e *microControllerError) Error() string { return e.err.Error() }

// Unwrap interface implementation
func (e *graphError) Unwrap() error           { return e.err }
func (e *crdError) Unwrap() error             { return e.err }
func (e *microControllerError) Unwrap() error { return e.err }

// Error constructors
func newGraphError(err error) error           { return &graphError{err} }
func newCRDError(err error) error             { return &crdError{err} }
func newMicroControllerError(err error) error { return &microControllerError{err} }

// listGraphRevisions returns the graph revisions for an RGD, split into live
// and terminating sets.
//
// GraphRevisions are grouped by RGD name (not UID) so a recreated RGD with the
// same name can adopt the existing revisions. The caller uses the terminating
// flag to defer decisions while GC is in flight.
func (r *ResourceGraphDefinitionReconciler) listGraphRevisions(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) (live []internalv1alpha1.GraphRevision, hasTerminating bool, err error) {
	revisionList := &internalv1alpha1.GraphRevisionList{}
	if err := r.List(ctx, revisionList, client.MatchingFields{
		"spec.snapshot.name": rgd.Name,
	}); err != nil {
		return nil, false, err
	}

	live = make([]internalv1alpha1.GraphRevision, 0, len(revisionList.Items))
	for i := range revisionList.Items {
		if revisionList.Items[i].GetDeletionTimestamp().IsZero() {
			live = append(live, revisionList.Items[i])
		} else {
			hasTerminating = true
		}
	}

	return live, hasTerminating, nil
}

type latestGraphRevisionView struct {
	RevisionNumber int64
	Revision       *internalv1alpha1.GraphRevision
	RuntimeEntry   *revisions.Entry
}

// getLatestGraphRevisionView returns the latest revision view and whether the
// latest revision is present in the in-memory registry.
//
// Only the latest revision needs to be warmed because the reconcile loop only
// ever serves the latest compiled graph. Older revisions may still be compiling
// in the GR controller — that's fine, they don't block serving or issuance.
func (r *ResourceGraphDefinitionReconciler) getLatestGraphRevisionView(
	rgdName string,
	graphRevisions []internalv1alpha1.GraphRevision,
) (latestGraphRevisionView, bool) {
	if len(graphRevisions) == 0 {
		return latestGraphRevisionView{}, true
	}

	latestIdx := 0
	for i := range graphRevisions {
		if graphRevisions[i].Spec.Revision > graphRevisions[latestIdx].Spec.Revision {
			latestIdx = i
		}
	}

	latestRevision := graphRevisions[latestIdx].Spec.Revision
	entry, ok := r.revisionsRegistry.Get(rgdName, latestRevision)
	if !ok {
		return latestGraphRevisionView{}, false
	}

	return latestGraphRevisionView{
		RevisionNumber: latestRevision,
		Revision:       &graphRevisions[latestIdx],
		RuntimeEntry:   &entry,
	}, true
}

// createGraphRevision creates a new GraphRevision object and seeds the
// in-memory registry entry as Pending.
//
// GraphRevisions are immutable snapshots of the RGD spec. The revision number
// is monotonically increasing per RGD name, and the spec hash is stored
// for duplicate-detection on subsequent reconciles.
func (r *ResourceGraphDefinitionReconciler) createGraphRevision(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	revision int64,
	specHash string,
) (*internalv1alpha1.GraphRevision, error) {
	// GraphRevisions are tracked by RGD name, so the UID label is not needed.
	// The spec hash label is kept for user-facing queries (kubectl).
	rgdNameLabeler := metadata.GenericLabeler{
		metadata.ResourceGraphDefinitionNameLabel: rgd.Name,
	}
	graphRevisionLabeler, err := r.metadataLabeler.Merge(rgdNameLabeler)
	if err != nil {
		return nil, fmt.Errorf("failed to setup graph revision labels: %w", err)
	}
	graphRevisionLabeler, err = graphRevisionLabeler.Merge(metadata.NewGraphRevisionHashLabeler(specHash))
	if err != nil {
		return nil, fmt.Errorf("failed to setup graph revision hash label: %w", err)
	}

	graphRevision := &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: graphRevisionName(rgd.Name, revision),
			OwnerReferences: []metav1.OwnerReference{
				metadata.NewResourceGraphDefinitionOwnerReference(rgd.Name, rgd.UID),
			},
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: revision,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name:       rgd.Name,
				Generation: rgd.Generation,
				Spec:       *rgd.Spec.DeepCopy(),
			},
		},
	}
	graphRevisionLabeler.ApplyLabels(&graphRevision.ObjectMeta)

	if err := r.Create(ctx, graphRevision); err != nil {
		return nil, fmt.Errorf("creating graph revision %q: %w", graphRevision.Name, err)
	}
	r.revisionsRegistry.Put(revisions.Entry{
		RGDName:  rgd.Name,
		Revision: revision,
		SpecHash: specHash,
		State:    revisions.RevisionStatePending,
	})
	return graphRevision, nil
}

// graphRevisionName generates a deterministic name for a GraphRevision object.
//
// Naming follows the Knative ChildName pattern (used by Knative, Tekton, and
// cert-manager): clean names for the common case, hash-based overflow for long
// names. This was chosen over alternatives after surveying the ecosystem:
//
//	| Project              | Format                    | Conditional | Content-addressable |
//	|----------------------|---------------------------|-------------|---------------------|
//	| K8s Deployment→RS    | {name}-{hash}             | Yes         | Yes                 |
//	| K8s ControllerRevision | {name}-{hash}           | No          | Yes                 |
//	| Knative Revision     | {name}-{gen:05d}          | Yes         | No (sequential)     |
//	| Crossplane           | {name}-{sha256[:7]}       | No          | Yes                 |
//	| Helm                 | ...{name}.v{n}            | No          | No (sequential)     |
//	| kro GraphRevision    | {name}-r{rev:05d}         | Yes         | No (sequential)     |
//
// kro's GraphRevisions are sequential immutable snapshots (like Knative), not
// content-addressable (like Deployments/Crossplane). Content-addressable naming
// is wrong here because reverts create new GRs — two GRs with identical specs
// but different revision numbers are distinct history points.
//
// The zero-padded revision number gives free lexicographic sorting in kubectl
// (up to 99,999 revisions per RGD).
//
// Common case (name + suffix ≤ 253):
//
//	my-webapp-r00001
//
// Overflow case (name would exceed 253):
//
//	{truncated-name}-r00001-{fnv32a(rgdName)}
//
// The hash suffix is only appended when truncation occurs, and is computed from
// the full untruncated RGD name. This ensures two different RGD names that
// truncate to the same prefix still produce unique revision names.
func graphRevisionName(rgdName string, revision int64) string {
	suffix := fmt.Sprintf("-r%05d", revision)

	// Common case: name fits without truncation — no hash needed.
	if len(rgdName)+len(suffix) <= 253 {
		return rgdName + suffix
	}

	// Overflow: truncate name and append hash for collision resistance.
	hash := graphRevisionNameHash(rgdName)
	suffix = fmt.Sprintf("-r%05d-%s", revision, hash)
	rgdName = rgdName[:253-len(suffix)]

	// Trim trailing dots/hyphens so the joined name stays DNS-1123 valid.
	// RGD names can contain dots (DNS-1123 subdomains allow them), so
	// truncation can land on a dot, producing "name.-r00001-hash" which is
	// invalid.
	rgdName = strings.TrimRight(rgdName, ".-")

	// Guard against degenerate names that are entirely dots/hyphens after
	// truncation (e.g. "a.---..." repeated 240 times).
	if len(rgdName) == 0 {
		rgdName = "gr"
	}

	return rgdName + suffix
}

// graphRevisionNameHash computes an FNV-32a hash of the RGD name for use as a
// collision guard in GraphRevision names during truncation overflow.
//
// This hash is only appended when the RGD name exceeds the Kubernetes 253-char
// limit. It is based on the full, untruncated RGD name so that different RGD
// names that truncate to the same prefix still produce unique revision names.
func graphRevisionNameHash(rgdName string) string {
	h := fnv.New32a()
	h.Write([]byte(rgdName))
	return hex.EncodeToString(h.Sum(nil))
}

// garbageCollectGraphRevisions deletes old GraphRevision objects exceeding the
// configured retention limit.
//
// Bounded retention GC keeps only the N most recent revisions per RGD,
// pruning both API objects and in-memory cache entries.
//
// TODO(amine): enforce a minimum retention of 2 (n-1) so there is always a
// rollback target if the latest revision fails.
func (r *ResourceGraphDefinitionReconciler) garbageCollectGraphRevisions(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	if r.cfg.MaxGraphRevisions <= 0 {
		return nil
	}

	graphRevisions, _, err := r.listGraphRevisions(ctx, rgd)
	if err != nil {
		return fmt.Errorf("listing graph revisions for gc: %w", err)
	}
	if len(graphRevisions) <= r.cfg.MaxGraphRevisions {
		return nil
	}

	minRevisionToKeep, ok := graphRevisionRetentionFloor(graphRevisions, r.cfg.MaxGraphRevisions)
	if !ok {
		return nil
	}
	for i := range graphRevisions {
		revision := graphRevisions[i]
		if revision.Spec.Revision >= minRevisionToKeep {
			continue
		}
		if err := r.Delete(ctx, &revision); err != nil {
			ignoreNotFound := client.IgnoreNotFound(err)
			if ignoreNotFound != nil {
				return fmt.Errorf("deleting graph revision %q: %w", revision.Name, err)
			}
		}
	}

	// Registry eviction is handled by the GR controller's finalizer as each
	// revision is deleted. No need to eagerly evict here.
	return nil
}

func graphRevisionRetentionFloor(revisions []internalv1alpha1.GraphRevision, maxGraphRevisions int) (int64, bool) {
	if maxGraphRevisions <= 0 || len(revisions) == 0 || len(revisions) < maxGraphRevisions {
		return 0, false
	}

	revisionNumbers := make([]int64, len(revisions))
	for i := range revisions {
		revisionNumbers[i] = revisions[i].Spec.Revision
	}
	slices.Sort(revisionNumbers)
	return revisionNumbers[len(revisionNumbers)-maxGraphRevisions], true
}
