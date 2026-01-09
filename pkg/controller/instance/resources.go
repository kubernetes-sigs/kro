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

package instance

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

func (c *Controller) reconcileResources(rcx *ReconcileContext) error {
	rcx.Log.V(2).Info("Reconciling resources")

	order := rcx.Runtime.TopologicalOrder()

	// ---------------------------------------------------------
	// 1. Prepare ApplySet
	// ---------------------------------------------------------
	applySet, err := c.createApplySet(rcx)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("applyset setup failed: %w", err))
	}

	// Values updated during the loop
	var unresolved string
	prune := true

	// ---------------------------------------------------------
	// 2. Reconcile each resource in topological order
	// ---------------------------------------------------------
	for _, id := range order {
		if err := c.reconcileResource(rcx, applySet, id, &unresolved, &prune); err != nil {
			return err
		}
	}

	// ---------------------------------------------------------
	// 3. Apply all accumulated changes
	// ---------------------------------------------------------
	result, err := applySet.Apply(rcx.Ctx, prune)
	if err != nil {
		return rcx.delayedRequeue(fmt.Errorf("apply/prune failed: %w", err))
	}

	// ---------------------------------------------------------
	// 4. Process results and update runtime state
	// ---------------------------------------------------------
	if err := c.processApplyResults(rcx, result); err != nil {
		return rcx.delayedRequeue(err)
	}

	// ---------------------------------------------------------
	// 5. Final resolution checks
	// ---------------------------------------------------------
	if unresolved != "" {
		return rcx.delayedRequeue(fmt.Errorf("waiting for unresolved resource %q", unresolved))
	}
	if result.HasClusterMutation() {
		/* We must requeue after cluster mutation so CEL values re-evaluate. */
		return rcx.delayedRequeue(fmt.Errorf("cluster mutated"))
	}

	return nil
}

func (c *Controller) reconcileResource(
	rcx *ReconcileContext,
	aset applyset.Set,
	id string,
	unresolved *string,
	prune *bool,
) error {
	rcx.Log.V(3).Info("Reconciling resource", "id", id)

	st := &ResourceState{State: ResourceStateInProgress}
	rcx.StateManager.ResourceStates[id] = st

	// 1. Should we process?
	want, err := rcx.Runtime.ReadyToProcessResource(id)
	if err != nil || !want {
		st.State = ResourceStateSkipped
		rcx.Log.V(2).Info("Skipping resource", "id", id, "reason", err)
		rcx.Runtime.IgnoreResource(id)
		return nil
	}

	// 2. Must be resolved
	_, rstate := rcx.Runtime.GetResource(id)
	if rstate != runtime.ResourceStateResolved {
		*unresolved = id
		*prune = false
		return nil
	}

	// 3. Dependencies must be ready
	if !rcx.Runtime.AreDependenciesReady(id) {
		*unresolved = id
		*prune = false
		return nil
	}

	// 4. External reference
	if rcx.Runtime.ResourceDescriptor(id).IsExternalRef() {
		return c.handleExternalRef(rcx, id, st)
	}

	// 5. Collection resource
	if rcx.Runtime.ResourceDescriptor(id).IsCollection() {
		return c.handleCollectionResource(rcx, aset, id, st, unresolved, prune)
	}

	// 6. Regular resource via ApplySet
	return c.handleApplySetResource(rcx, aset, id, st)
}

func (c *Controller) createApplySet(rcx *ReconcileContext) (applyset.Set, error) {
	lbl, err := metadata.NewInstanceLabeler(rcx.Runtime.GetInstance()).
		Merge(rcx.Labeler)
	if err != nil {
		return nil, fmt.Errorf("labeler merge: %w", err)
	}

	cfg := applyset.Config{
		ToolLabels:   lbl.Labels(),
		FieldManager: FieldManagerForApplyset,
		ToolingID:    KROTooling,
		Log:          rcx.Log,
	}

	return applyset.New(rcx.Runtime.GetInstance(), rcx.RestMapper, rcx.Client, cfg)
}

func (c *Controller) handleApplySetResource(
	rcx *ReconcileContext,
	aset applyset.Set,
	id string,
	st *ResourceState,
) error {
	desired, _ := rcx.Runtime.GetResource(id)

	applyable := applyset.ApplyableObject{
		Unstructured: desired,
		ID:           id,
	}

	actual, err := aset.Add(rcx.Ctx, applyable)
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return err
	}

	if actual != nil {
		rcx.Runtime.SetResource(id, actual)
		updateReadiness(rcx, id)
		if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
			return err
		}
	}
	return nil
}

func updateReadiness(rcx *ReconcileContext, id string) {
	st := rcx.StateManager.ResourceStates[id]

	ready, reason, err := rcx.Runtime.IsResourceReady(id)
	if err != nil || !ready {
		st.State = ResourceStateWaitingForReadiness
		st.Err = fmt.Errorf("not ready: %s: %w", reason, err)
	} else {
		st.State = ResourceStateSynced
	}
}

func (c *Controller) handleCollectionResource(
	rcx *ReconcileContext,
	aset applyset.Set,
	id string,
	st *ResourceState,
	unresolved *string,
	prune *bool,
) error {
	expandedResources, err := rcx.Runtime.ExpandCollection(id)
	if err != nil {
		st.State = ResourceStateError
		st.Err = fmt.Errorf("failed to expand collection: %w", err)
		*unresolved = id
		*prune = false
		return nil
	}

	collectionSize := len(expandedResources)
	collectionResults := make([]*unstructured.Unstructured, collectionSize)
	for i, expandedResource := range expandedResources {
		collectionLabeler := metadata.NewCollectionItemLabeler(id, i, collectionSize)
		collectionLabeler.ApplyLabels(expandedResource)
		expandedID := fmt.Sprintf("%s-%d", id, i)
		clusterObj, err := aset.Add(rcx.Ctx, applyset.ApplyableObject{Unstructured: expandedResource, ID: expandedID})
		if err != nil {
			return fmt.Errorf("failed to add expanded resource %s to applyset: %w", expandedID, err)
		}
		collectionResults[i] = clusterObj
	}

	// Set collection resources and check readiness
	rcx.Runtime.SetCollectionResources(id, collectionResults)
	if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return fmt.Errorf("failed to synchronize after expanding collection %s: %w", id, err)
	}

	// Check readiness - if not ready, we'll check again after apply with observed objects
	if ready, reason, err := rcx.Runtime.IsCollectionReady(id); err != nil || !ready {
		rcx.Log.V(1).Info("Collection not ready", "id", id, "reason", reason, "error", err)
		st.State = ResourceStateWaitingForReadiness
		if err != nil {
			st.Err = err
		}
	}
	return nil
}

func (c *Controller) handleExternalRef(
	rcx *ReconcileContext,
	id string,
	st *ResourceState,
) error {
	desired, _ := rcx.Runtime.GetResource(id)

	actual, err := c.readExternalRef(rcx, id, desired)
	if err != nil {
		st.State = ResourceStateError
		st.Err = err
		return nil
	}

	rcx.Runtime.SetResource(id, actual)
	updateReadiness(rcx, id)
	if _, err = rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return err
	}
	return nil
}

func (c *Controller) readExternalRef(
	rcx *ReconcileContext,
	resourceID string,
	desired *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {

	gvk := desired.GroupVersionKind()

	// 1. Map GVK → GVR
	mapping, err := c.client.RESTMapper().RESTMapping(
		gvk.GroupKind(),
		gvk.Version,
	)
	if err != nil {
		return nil, fmt.Errorf("externalRef: RESTMapping for %s: %w", gvk, err)
	}

	// 2. Determine which client to use
	var ri dynamic.ResourceInterface

	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := desired.GetNamespace()
		if ns == "" {
			ns = rcx.getResourceNamespace(resourceID)
		}
		ri = c.client.Dynamic().Resource(mapping.Resource).Namespace(ns)
	} else {
		ri = c.client.Dynamic().Resource(mapping.Resource)
	}

	// 3. Fetch existing object
	name := desired.GetName()

	obj, err := ri.Get(rcx.Ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("externalRef: GET %s %s/%s: %w",
			gvk.String(), desired.GetNamespace(), name, err,
		)
	}

	rcx.Log.Info("External reference resolved",
		"resourceID", resourceID,
		"gvk", gvk.String(),
		"namespace", obj.GetNamespace(),
		"name", obj.GetName(),
	)

	return obj, nil
}

func (c *Controller) processApplyResults(
	rcx *ReconcileContext,
	result *applyset.ApplyResult,
) error {

	rcx.Log.V(2).Info("Processing apply results")

	// Build maps for efficient lookup
	appliedByID := make(map[string]*unstructured.Unstructured)
	errorsByID := make(map[string]error)
	for _, applied := range result.AppliedObjects {
		if applied.Error != nil {
			errorsByID[applied.ID] = applied.Error
		} else if applied.LastApplied != nil {
			appliedByID[applied.ID] = applied.LastApplied
		}
	}

	// Process all resources from apply results
	for resourceID, resourceState := range rcx.StateManager.ResourceStates {
		if rcx.Runtime.ResourceDescriptor(resourceID).IsCollection() {
			if err := c.updateCollectionFromApplyResults(rcx, resourceID, resourceState, appliedByID, errorsByID); err != nil {
				return err
			}
		} else {
			// Regular resources
			if err, hasErr := errorsByID[resourceID]; hasErr {
				resourceState.State = ResourceStateError
				resourceState.Err = err
				rcx.Log.V(1).Info("apply error", "id", resourceID, "error", err)
			} else if applied, ok := appliedByID[resourceID]; ok {
				rcx.Runtime.SetResource(resourceID, applied)
				updateReadiness(rcx, resourceID)

				if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
					resourceState.State = ResourceStateError
					resourceState.Err = fmt.Errorf("failed to synchronize after apply: %w", err)
					continue
				}
			}
		}
	}

	// ---------------------------------------------------------
	// Aggregate all resource errors
	// ---------------------------------------------------------
	if err := rcx.StateManager.ResourceErrors(); err != nil {
		return fmt.Errorf("apply results contain errors: %w", err)
	}

	return nil
}

func (c *Controller) updateCollectionFromApplyResults(
	rcx *ReconcileContext,
	resourceID string,
	resourceState *ResourceState,
	appliedByID map[string]*unstructured.Unstructured,
	errorsByID map[string]error,
) error {
	currentResources, state := rcx.Runtime.GetCollectionResources(resourceID)
	if state != runtime.ResourceStateResolved || currentResources == nil {
		return nil
	}

	for i := range currentResources {
		expandedID := fmt.Sprintf("%s-%d", resourceID, i)
		if err, hasErr := errorsByID[expandedID]; hasErr {
			resourceState.State = ResourceStateError
			resourceState.Err = fmt.Errorf("collection item %d: %w", i, err)
			return nil
		}
		if applied, ok := appliedByID[expandedID]; ok {
			currentResources[i] = applied
		}
	}

	rcx.Runtime.SetCollectionResources(resourceID, currentResources)
	if _, err := rcx.Runtime.Synchronize(); err != nil && !runtime.IsDataPending(err) {
		return fmt.Errorf("failed to synchronize after applying collection %s: %w", resourceID, err)
	}

	// Check collection readiness
	if ready, reason, err := rcx.Runtime.IsCollectionReady(resourceID); err != nil || !ready {
		rcx.Log.V(1).Info("Collection not ready", "resourceID", resourceID, "reason", reason, "error", err)
		resourceState.State = ResourceStateWaitingForReadiness
		resourceState.Err = fmt.Errorf("collection not ready: %s: %w", reason, err)
	} else {
		resourceState.State = ResourceStateSynced
	}
	return nil
}
