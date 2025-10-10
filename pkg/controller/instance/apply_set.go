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
	"context"
	"fmt"

	"github.com/kubernetes-sigs/kro/pkg/applyset"
	"github.com/kubernetes-sigs/kro/pkg/runtime"
	"sigs.k8s.io/release-utils/version"
)

const FieldManagerForApplyset = "kro.run/applyset"

var (
	KROTooling = applyset.ToolingID{
		Name:    "kro",
		Version: version.GetVersionInfo().GitVersion,
	}
)

// reconcileInstanceApplySet handles the reconciliation of an active instance
func (igr *instanceGraphReconciler) reconcileInstanceApplySet(ctx context.Context) error {
	instance := igr.runtime.GetInstance()

	// Set managed state and handle instance labels
	if err := igr.setupInstance(ctx, instance); err != nil {
		return fmt.Errorf("failed to setup instance: %w", err)
	}

	// Initialize resource states
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		igr.state.ResourceStates[resourceID] = &ResourceState{State: ResourceStatePending}
	}

	config := applyset.Config{
		ToolLabels:   igr.instanceSubResourcesLabeler.Labels(),
		FieldManager: FieldManagerForApplyset,
		ToolingID:    KROTooling,
		Log:          igr.log,
	}

	aset, err := applyset.New(instance, igr.restMapper, igr.client, config)
	if err != nil {
		return igr.delayedRequeue(fmt.Errorf("failed creating an applyset: %w", err))
	}

	unresolvedResourceID := ""
	prune := true
	// Reconcile resources in topological order
	for _, resourceID := range igr.runtime.TopologicalOrder() {
		log := igr.log.WithValues("resourceID", resourceID)

		// Initialize resource state in instance state
		resourceState := &ResourceState{State: ResourceStateInProgress}
		igr.state.ResourceStates[resourceID] = resourceState

		// Check if resource should be processed (create or get)
		// TODO(barney-s): skipping on error seems un-intuitive, should we skip on CEL evaluation error?
		if want, err := igr.runtime.ReadyToProcessResource(resourceID); err != nil || !want {
			log.V(1).Info("Skipping resource processing", "reason", err)
			resourceState.State = ResourceStateSkipped
			igr.runtime.IgnoreResource(resourceID)
			continue
		}

		// Check if the resource dependencies are resolved and can be reconciled
		resource, state := igr.runtime.GetResource(resourceID)

		if state != runtime.ResourceStateResolved {
			unresolvedResourceID = resourceID
			prune = false
			break
		}

		applyable := applyset.ApplyableObject{
			Unstructured: resource,
			ID:           resourceID,
			ExternalRef:  igr.runtime.ResourceDescriptor(resourceID).IsExternalRef(),
		}
		clusterObj, err := aset.Add(ctx, applyable)
		if err != nil {
			return fmt.Errorf("failed to add resource to applyset: %w", err)
		}

		if clusterObj != nil {
			igr.runtime.SetResource(resourceID, clusterObj)
			igr.updateResourceReadiness(resourceID)
			// Synchronize runtime state after each resource
			if _, err := igr.runtime.Synchronize(); err != nil {
				return fmt.Errorf("failed to synchronize after apply/prune: %w", err)
			}
		}
	}

	result, err := aset.Apply(ctx, prune)
	for _, applied := range result.AppliedObjects {
		resourceState := igr.state.ResourceStates[applied.ID]
		if applied.Error != nil {
			resourceState.State = ResourceStateError
			resourceState.Err = applied.Error
		} else {
			igr.updateResourceReadiness(applied.ID)
		}
	}

	if err != nil {
		return igr.delayedRequeue(fmt.Errorf("failed to apply/prune resources: %w", err))
	}

	// Inspect resource states and return error if any resource is in error state
	if err := igr.state.ResourceErrors(); err != nil {
		return igr.delayedRequeue(err)
	}

	if err := result.Errors(); err != nil {
		return fmt.Errorf("failed to apply/prune resources: %w", err)
	}

	if unresolvedResourceID != "" {
		return igr.delayedRequeue(fmt.Errorf("unresolved resource: %s", unresolvedResourceID))
	}

	// If there are any cluster mutations, we need to requeue.
	if result.HasClusterMutation() {
		return igr.delayedRequeue(fmt.Errorf("changes applied to cluster"))
	}

	return nil
}
