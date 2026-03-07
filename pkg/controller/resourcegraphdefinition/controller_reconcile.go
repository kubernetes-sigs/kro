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
	"fmt"
	"slices"
	"time"

	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	instancectrl "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	graphhash "github.com/kubernetes-sigs/kro/pkg/graph/hash"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// reconcileResourceGraphDefinition orchestrates the reconciliation of a ResourceGraphDefinition by:
// 1. Processing the resource graph
// 2. Ensuring CRDs are present
// 3. Setting up and starting the microcontroller
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinition(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
) (ctrl.Result, []string, []v1alpha1.ResourceInformation, error) {
	log := ctrl.LoggerFrom(ctx)
	mark := NewConditionsMarkerFor(rgd)
	defer func() {
		err := r.garbageCollectGraphRevisions(ctx, rgd)
		if err != nil {
			log.Error(err, "failed to garbage collect graph revisions", "name", rgd.Name)
		}
	}()

	graphRevisions, err := r.listGraphRevisions(ctx, rgd)
	if err != nil {
		return ctrl.Result{}, nil, nil, fmt.Errorf("listing graph revisions: %w", err)
	}
	cacheWarmed := r.isRevisionCacheWarmed(rgd.Name, graphRevisions)
	if !cacheWarmed {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
	}

	currentSpecHash, err := graphhash.Spec(rgd.Spec)
	if err != nil {
		mark.ResourceGraphInvalid(err.Error())
		return ctrl.Result{}, nil, nil, err
	}

	latestRevision := getLatestGraphRevision(graphRevisions)

	var processedRGD *graph.Graph
	var resourcesInfo []v1alpha1.ResourceInformation

	if latestRevision != nil && latestRevision.Spec.SpecHash == currentSpecHash {
		rgd.Status.LatestObservedGV = graphRevisionReference(*latestRevision)
		rgd.Status.LastIssuedRevision = maxInt64(rgd.Status.LastIssuedRevision, latestRevision.Spec.Revision)

		cachedEntry, ok := r.revisionsRegistry.Get(rgd.Name, latestRevision.Spec.Revision)
		if !ok {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		}

		// Hash match only answers issuance ("should we create another GV?").
		// Serving behavior is decided by the revision state in cache.
		switch cachedEntry.State {
		case revisions.RevisionStateActive:
			// Active means GV compilation succeeded and the compiled graph is
			// available by invariant.
			processedRGD = cachedEntry.CompiledGraph
			resourcesInfo = resourcesInfoFromGraph(processedRGD)
			mark.ResourceGraphValid()
		case revisions.RevisionStatePending:
			// Same spec hash, but compilation is still in-flight.
			return ctrl.Result{RequeueAfter: 2 * time.Second}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		case revisions.RevisionStateFailed:
			// Same spec hash, but the latest issued revision failed compilation.
			mark.ResourceGraphInvalid(fmt.Sprintf("latest graph revision %d failed", latestRevision.Spec.Revision))
			return ctrl.Result{}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		default:
			return ctrl.Result{RequeueAfter: 2 * time.Second}, rgd.Status.TopologicalOrder, rgd.Status.Resources, nil
		}
	} else {
		log.V(1).Info("reconciling resource graph definition graph")
		processedRGD, resourcesInfo, err = r.reconcileResourceGraphDefinitionGraph(ctx, rgd)
		if err != nil {
			mark.ResourceGraphInvalid(err.Error())
			return ctrl.Result{}, nil, nil, err
		}
		mark.ResourceGraphValid()

		revision := maxInt64(rgd.Status.LastIssuedRevision, latestRevisionNumber(latestRevision)) + 1
		createdGV, createErr := r.createGraphRevision(ctx, rgd, revision, currentSpecHash)
		if createErr != nil {
			return ctrl.Result{}, nil, nil, createErr
		}
		rgd.Status.LatestObservedGV = graphRevisionReference(*createdGV)
		rgd.Status.LastIssuedRevision = createdGV.Spec.Revision
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
	if rgd.Status.LatestObservedGV != nil {
		v := *rgd.Status.LatestObservedGV
		rgd.Status.LatestActiveGV = &v
	}

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
// and extract resource information
func (r *ResourceGraphDefinitionReconciler) reconcileResourceGraphDefinitionGraph(_ context.Context, rgd *v1alpha1.ResourceGraphDefinition) (*graph.Graph, []v1alpha1.ResourceInformation, error) {
	processedRGD, err := r.rgBuilder.NewResourceGraphDefinition(rgd, r.rgdConfig)
	if err != nil {
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
	for name, node := range processedRGD.Nodes {
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

func (r *ResourceGraphDefinitionReconciler) listGraphRevisions(ctx context.Context, rgd *v1alpha1.ResourceGraphDefinition) ([]v1alpha1.GraphRevision, error) {
	labelSelector := labels.SelectorFromSet(map[string]string{
		metadata.ResourceGraphDefinitionNameLabel: rgd.Name,
	})

	revisionList := &v1alpha1.GraphRevisionList{}
	if err := r.List(ctx, revisionList, &client.ListOptions{LabelSelector: labelSelector}); err != nil {
		return nil, err
	}

	return revisionList.Items, nil
}

func (r *ResourceGraphDefinitionReconciler) isRevisionCacheWarmed(rgdName string, revisions []v1alpha1.GraphRevision) bool {
	// Warmup is about membership only: all listed GV numbers are represented in
	// memory. State handling (Pending/Failed/Active) happens later per latest GV.
	revisionNumbers := make([]int64, len(revisions))
	for i := range revisions {
		revisionNumbers[i] = revisions[i].Spec.Revision
	}
	return r.revisionsRegistry.HasAll(rgdName, revisionNumbers)
}

func getLatestGraphRevision(revisions []v1alpha1.GraphRevision) *v1alpha1.GraphRevision {
	if len(revisions) == 0 {
		return nil
	}
	latestIdx := 0
	for i := 1; i < len(revisions); i++ {
		if revisions[i].Spec.Revision > revisions[latestIdx].Spec.Revision {
			latestIdx = i
		}
	}
	return &revisions[latestIdx]
}

func latestRevisionNumber(latest *v1alpha1.GraphRevision) int64 {
	if latest == nil {
		return 0
	}
	return latest.Spec.Revision
}

func graphRevisionReference(revision v1alpha1.GraphRevision) *v1alpha1.GraphRevisionReference {
	return &v1alpha1.GraphRevisionReference{
		Name:     revision.Name,
		Revision: revision.Spec.Revision,
		SpecHash: revision.Spec.SpecHash,
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (r *ResourceGraphDefinitionReconciler) createGraphRevision(
	ctx context.Context,
	rgd *v1alpha1.ResourceGraphDefinition,
	revision int64,
	specHash string,
) (*v1alpha1.GraphRevision, error) {
	rgdLabeler := metadata.NewResourceGraphDefinitionLabeler(rgd)
	graphRevisionLabeler, err := r.metadataLabeler.Merge(rgdLabeler)
	if err != nil {
		return nil, fmt.Errorf("failed to setup graph revision labels: %w", err)
	}

	graphRevision := &v1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: graphRevisionName(rgd.Name, revision),
			OwnerReferences: []metav1.OwnerReference{
				metadata.NewResourceGraphDefinitionOwnerReference(rgd.Name, rgd.UID),
			},
		},
		Spec: v1alpha1.GraphRevisionSpec{
			ResourceGraphDefinitionName: rgd.Name,
			ResourceGraphDefinitionUID:  rgd.UID,
			Revision:                    revision,
			SpecHash:                    specHash,
			DefinitionSpec:              *rgd.Spec.DeepCopy(),
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
	suffix := fmt.Sprintf("-r%d", revision)
	maxPrefixLen := 253 - len(suffix)
	if len(rgdName) > maxPrefixLen {
		rgdName = rgdName[:maxPrefixLen]
	}
	return rgdName + suffix
}

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

	r.revisionsRegistry.DeleteBelow(rgd.Name, minRevisionToKeep)
	return nil
}

func graphRevisionRetentionFloor(revisions []v1alpha1.GraphRevision, maxGraphRevisions int) int64 {
	revisionNumbers := make([]int64, len(revisions))
	for i := range revisions {
		revisionNumbers[i] = revisions[i].Spec.Revision
	}
	slices.Sort(revisionNumbers)
	return revisionNumbers[len(revisionNumbers)-maxGraphRevisions]
}
