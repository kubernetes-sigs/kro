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

const (
	// graphRevisionGCTimeout is the maximum time allowed for garbage collecting
	// old GraphRevision objects after each reconciliation.
	graphRevisionGCTimeout = 60 * time.Second
	// defaultRequeueDelay is the delay before requeuing when waiting for
	// asynchronous state to converge (e.g. cache warmup, pending compilation).
	defaultRequeueDelay = 3 * time.Second
	// graphRevisionNameHashLen is the short hash suffix length in the GraphRevision
	// name. This balances readability with collision resistance after truncation.
	graphRevisionNameHashLen = 12
)

// reconcileResourceGraphDefinition orchestrates the reconciliation of a ResourceGraphDefinition.
//
// See KREP-013 for the full design.
//  1. List existing GraphRevisions and ensure the in-memory registry is warmed.
//  2. Hash the current RGD spec and compare against the latest issued revision.
//  3. If unchanged, resolve the compiled graph from cache; if changed, compile
//     and issue a new GraphRevision.
//  4. Ensure the CRD exists and start/update the micro-controller.
//  5. Garbage-collect old GraphRevisions per --rgd-max-graph-revisions.
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinition(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, []string, []v1alpha1.ResourceInformation, error) {
	log := ctrl.LoggerFrom(ctx)
	mark := NewConditionsMarkerFor(rgd)
	defer func() {
		// Use a dedicated timeout context for GC so it isn't cancelled when
		// the reconciliation context is done (e.g. during controller shutdown).
		gcCtx, cancel := context.WithTimeout(context.Background(), graphRevisionGCTimeout)
		defer cancel()
		err := r.garbageCollectGraphRevisions(gcCtx, rgd)
		if err != nil {
			mark.GraphRevisionGCUnhealthy(err.Error())
			log.Error(err, "failed to garbage collect graph revisions", "name", rgd.Name)
			return
		}
		mark.GraphRevisionGCHealthy()
	}()

	// Before making any issuance or serving decision, ensure all live
	// GraphRevision objects are represented in the in-memory registry.
	// This guards against stale decisions after a controller restart.
	graphRevisions, err := r.listGraphRevisions(ctx, rgd)
	if err != nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("listing graph revisions: %w", err)
	}
	latestRevisionView, warmed := r.getLatestGraphRevisionView(rgd.Name, graphRevisions)
	if !warmed {
		return ctrl.Result{RequeueAfter: defaultRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
	}

	currentSpecHash, err := graphhash.Spec(rgd.Spec)
	if err != nil {
		mark.ResourceGraphInvalid(err.Error())
		return ctrl.Result{}, nil, nil, err
	}
	latestRevision := latestRevisionView.Revision

	var processedRGD *graph.Graph
	var resourcesInfo []v1alpha1.ResourceInformation

	// A new GraphRevision is issued only when the spec hash changes.
	// Otherwise we resolve the compiled graph from the existing revision.
	if latestRevision != nil && latestRevision.Spec.Snapshot.SpecHash == currentSpecHash {
		rgd.Status.LastIssuedRevision = max(rgd.Status.LastIssuedRevision, latestRevision.Spec.Revision)

		if latestRevisionView.RuntimeEntry == nil {
			return ctrl.Result{RequeueAfter: defaultRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		}

		// Hash match only answers issuance ("should we create another GR?").
		// Serving behavior is decided by the revision state in cache.
		switch latestRevisionView.RuntimeEntry.State {
		case revisions.RevisionStateActive:
			// Active means GR compilation succeeded and the compiled graph is
			// available by invariant.
			processedRGD = latestRevisionView.RuntimeEntry.CompiledGraph
			resourcesInfo = resourcesInfoFromGraph(processedRGD)
			mark.ResourceGraphValid()
		case revisions.RevisionStatePending:
			// Same spec hash, but compilation is still in-flight.
			return ctrl.Result{RequeueAfter: defaultRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		case revisions.RevisionStateFailed:
			// Same spec hash, but the latest issued revision failed compilation.
			// Return an error to trigger exponential backoff requeue — the failure
			// may be transient (e.g. a dependency CRD was temporarily missing).
			failErr := fmt.Errorf("latest graph revision %d failed", latestRevision.Spec.Revision)
			mark.ResourceGraphInvalid(failErr.Error())
			return ctrl.Result{}, rgd.Status.TopologicalOrder, rgd.Status.Resources, failErr
		default:
			return ctrl.Result{RequeueAfter: defaultRequeueDelay}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		}
	} else {
		log.V(1).Info("reconciling resource graph definition graph")
		processedRGD, resourcesInfo, err = r.reconcileResourceGraphDefinitionGraph(ctx, rgd)
		if err != nil {
			mark.ResourceGraphInvalid(err.Error())
			return ctrl.Result{}, nil, nil, err
		}
		mark.ResourceGraphValid()

		// Watermark recovery: use the higher of the persisted high-water mark
		// and the highest observed revision from live GraphRevisions. This
		// handles status update failures and RGD recreation (where status is
		// reset but orphaned revisions may still exist).
		//
		// Edge case: if both LastIssuedRevision status update failed AND all
		// GRs were GC'd, revision resets to 1. A name collision with a
		// still-terminating old r1 would fail with AlreadyExists and requeue.
		revision := max(rgd.Status.LastIssuedRevision, latestRevisionView.RevisionNumber) + 1
		createdGR, createErr := r.createGraphRevision(ctx, rgd, revision, currentSpecHash)
		if createErr != nil {
			return ctrl.Result{}, nil, nil, createErr
		}
		rgd.Status.LastIssuedRevision = createdGR.Spec.Revision
	}

	// Build instance labeler: kro metadata + RGD-specific labels.
	// This is applied to CRDs and instances. Child resources only get r.metadataLabeler.
	rgdLabeler := metadata.NewResourceGraphDefinitionLabeler(rgd)
	instanceLabeler, err := r.metadataLabeler.Merge(rgdLabeler)
	if err != nil {
		mark.FailedLabelerSetup(err.Error())
		return ctrl.Result{}, nil, nil, fmt.Errorf("failed to setup labeler: %w", err)
	}

	crd := processedRGD.CRD
	instanceLabeler.ApplyLabels(&crd.ObjectMeta)

	// Ensure CRD exists and is up to date
	log.V(1).Info("reconciling resource graph definition CRD")
	allowBreakingChanges := rgd.Annotations[v1alpha1.AllowBreakingChangesAnnotation] == "true"
	if err := r.reconcileResourceGraphDefinitionCRD(ctx, crd, allowBreakingChanges); err != nil {
		mark.KindUnready(err.Error())
		return ctrl.Result{}, processedRGD.TopologicalOrder, resourcesInfo, err
	}
	if crd, err = r.crdManager.Get(ctx, crd.Name); err != nil {
		mark.KindUnready(err.Error())
	} else {
		mark.KindReady(crd.Status.AcceptedNames.Kind)
	}

	if err := r.reconcileResourceGraphDefinitionMicroController(ctx, rgd, processedRGD, instanceLabeler); err != nil {
		mark.ControllerFailedToStart(err.Error())
		return ctrl.Result{}, processedRGD.TopologicalOrder, resourcesInfo, err
	}
	mark.ControllerRunning()

	return ctrl.Result{}, processedRGD.TopologicalOrder, resourcesInfo, nil
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
			DefaultRequeueDuration:    3 * time.Second,
			DeletionGraceTimeDuration: 30 * time.Second,
			DeletionPolicy:            "Delete",
			RGDConfig:                 r.rgdConfig,
		},
		gvr,
		r.revisionsRegistry.ResolverForRGD(rgd.Name),
		r.clientSet,
		instanceLabeler,
		r.metadataLabeler,
		r.dynamicController.Coordinator(),
	)
}

// reconcileResourceGraphDefinitionGraph processes the resource graph definition to build a dependency graph
// and extract resource information. It skips the expensive build pipeline when the
// RGD's .metadata.generation has not changed since the last successful build.
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionGraph(_ context.Context, rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	startTime := time.Now()
	defer func() {
		graphBuildDuration.WithLabelValues(rgd.Spec.Schema.Kind).Observe(time.Since(startTime).Seconds())
		graphBuildTotal.WithLabelValues(rgd.Spec.Schema.Kind).Inc()
	}()

	processedRGD, err := r.rgBuilder.NewResourceGraphDefinition(rgd, r.rgdConfig)
	if err != nil {
		graphBuildErrorsTotal.WithLabelValues(rgd.Spec.Schema.Kind).Inc()
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

// reconcileResourceGraphDefinitionCRD ensures the CRD is present and up to date in the cluster
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionCRD(ctx context.Context, crd *v1.CustomResourceDefinition, allowBreakingChanges bool) error {
	if err := r.crdManager.Ensure(ctx, *crd, allowBreakingChanges); err != nil {
		return newCRDError(err)
	}
	return nil
}

// reconcileResourceGraphDefinitionMicroController starts the microcontroller for handling the resources.
// Child/external resource watches are discovered dynamically by the coordinator from
// Watch() calls made by instance reconcilers -- no GVR list needed here.
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionMicroController(
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

// listGraphRevisions returns the live revision lineage for an RGD by its name label.
//
// This is an intentional policy choice: GraphRevisions are grouped by RGD name
// so an RGD deleted and recreated with the same name can adopt the existing
// revision lineage instead of starting from scratch due only to a new object UID.
// Revisions already marked for deletion are excluded so retention GC does not
// block cache warmup or issuance decisions while old objects are terminating.
func (r *ResourceGraphDefinitionReconciler) listGraphRevisions(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) ([]internalv1alpha1.GraphRevision, error) {
	revisionList := &internalv1alpha1.GraphRevisionList{}
	if err := r.List(ctx, revisionList, client.MatchingFields{
		"spec.snapshot.name": rgd.Name,
	}); err != nil {
		return nil, err
	}

	liveRevisions := make([]internalv1alpha1.GraphRevision, 0, len(revisionList.Items))
	for i := range revisionList.Items {
		if revisionList.Items[i].GetDeletionTimestamp().IsZero() {
			liveRevisions = append(liveRevisions, revisionList.Items[i])
		}
	}

	return liveRevisions, nil
}

type latestGraphRevisionView struct {
	RevisionNumber int64
	Revision       *internalv1alpha1.GraphRevision
	RuntimeEntry   *revisions.Entry
}

// getLatestGraphRevisionView returns the latest revision view and whether the
// in-memory registry is fully warmed for this RGD lineage.
//
// Warmup is a membership check: all listed GR numbers must be present in memory
// before any issuance or serving decision. State handling (Pending/Failed/Active)
// is separate and happens per latest GR.
func (r *ResourceGraphDefinitionReconciler) getLatestGraphRevisionView(
	rgdName string,
	graphRevisions []internalv1alpha1.GraphRevision,
) (latestGraphRevisionView, bool) {
	if len(graphRevisions) == 0 {
		return latestGraphRevisionView{}, true
	}

	revisionNumbers := make([]int64, len(graphRevisions))
	latestIdx := 0
	for i := range graphRevisions {
		revisionNumbers[i] = graphRevisions[i].Spec.Revision
		if graphRevisions[i].Spec.Revision > graphRevisions[latestIdx].Spec.Revision {
			latestIdx = i
		}
	}

	if !r.revisionsRegistry.HasAll(rgdName, revisionNumbers) {
		return latestGraphRevisionView{}, false
	}

	view := latestGraphRevisionView{
		RevisionNumber: graphRevisions[latestIdx].Spec.Revision,
		Revision:       &graphRevisions[latestIdx],
	}
	if entry, ok := r.revisionsRegistry.Get(rgdName, view.Revision.Spec.Revision); ok {
		view.RuntimeEntry = &entry
	}

	return view, true
}

// createGraphRevision creates a new GraphRevision object and seeds the
// in-memory registry entry as Pending.
//
// GraphRevisions are immutable snapshots of the RGD spec. The revision number
// is monotonically increasing per RGD name lineage, and the spec hash is stored
// for duplicate-detection on subsequent reconciles.
func (r *ResourceGraphDefinitionReconciler) createGraphRevision(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	revision int64,
	specHash string,
) (*internalv1alpha1.GraphRevision, error) {
	// GraphRevisions use name-based lineage, so the UID label is not needed.
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
				UID:        rgd.UID,
				Generation: rgd.Generation,
				SpecHash:   specHash,
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

func graphRevisionName(rgdName string, revision int64) string {
	hash := graphRevisionNameHash(rgdName)
	suffix := fmt.Sprintf("-r%d-%s", revision, hash)
	maxPrefixLen := 253 - len(suffix)
	if len(rgdName) > maxPrefixLen {
		rgdName = rgdName[:maxPrefixLen]
	}
	// Trim trailing dots/hyphens so the joined name stays DNS-1123 valid.
	// RGD names can contain dots (DNS-1123 subdomains allow them), so
	// truncation can land on a dot, producing "name.-r1-hash" which is invalid.
	rgdName = strings.TrimRight(rgdName, ".-")
	return rgdName + suffix
}

// graphRevisionNameHash computes a short FNV-128a hash of the RGD name for use
// in GraphRevision object names.
//
// This hash exists solely as a collision guard for truncated RGD names. When an
// RGD name exceeds the Kubernetes 253-char name limit minus the suffix length,
// the name is truncated. Two different RGD names that truncate to the same
// prefix would produce identical revision names without this hash.
//
// The hash is based on the full, untruncated RGD name, so even truncated
// prefixes produce unique revision names. It is NOT a content hash — the same
// RGD always produces the same hash regardless of spec changes, giving
// consistent naming across revisions (e.g. my-webapp-r1-aabb, my-webapp-r2-aabb).
//
// FNV-128a is used instead of a cryptographic hash because this is a naming
// concern, not a security boundary. FNV is fast and well-distributed.
func graphRevisionNameHash(rgdName string) string {
	h := fnv.New128a()
	h.Write([]byte(rgdName))
	return hex.EncodeToString(h.Sum(nil))[:graphRevisionNameHashLen]
}

// garbageCollectGraphRevisions deletes old GraphRevision objects exceeding the
// configured retention limit.
//
// Bounded retention GC keeps only the N most recent revisions per RGD,
// pruning both API objects and in-memory cache entries.
func (r *ResourceGraphDefinitionReconciler) garbageCollectGraphRevisions(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) error {
	if r.maxGraphRevisions <= 0 {
		return nil
	}

	graphRevisions, err := r.listGraphRevisions(ctx, rgd)
	if err != nil {
		return fmt.Errorf("listing graph revisions for gc: %w", err)
	}
	if len(graphRevisions) <= r.maxGraphRevisions {
		return nil
	}

	minRevisionToKeep := graphRevisionRetentionFloor(graphRevisions, r.maxGraphRevisions)
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

	r.revisionsRegistry.DeleteRevisionsBefore(rgd.Name, minRevisionToKeep)
	return nil
}

func graphRevisionRetentionFloor(revisions []internalv1alpha1.GraphRevision, maxGraphRevisions int) int64 {
	revisionNumbers := make([]int64, len(revisions))
	for i := range revisions {
		revisionNumbers[i] = revisions[i].Spec.Revision
	}
	slices.Sort(revisionNumbers)
	return revisionNumbers[len(revisionNumbers)-maxGraphRevisions]
}
