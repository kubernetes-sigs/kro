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
	"encoding/json"
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	controllergraph "github.com/kubernetes-sigs/kro/pkg/controller/graph"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// reconcileDeletion drives deletion workflow for an instance.
func (c *Controller) reconcileDeletion(dcx *DeletionContext) error {
	dcx.State = v1alpha1.InstanceStateDeleting
	dcx.Mark.ResourcesUnderDeletion("deleting resources")

	candidates, applier, err := c.discoverDeletionInventory(dcx)
	if err != nil {
		dcx.Mark.ResourcesUnderDeletion("deletion blocked: %v", err)
		return err
	}

	if len(candidates) == 0 {
		return c.removeFinalizer(dcx)
	}

	wave, highest := highestDeletionWave(candidates)

	conflict := false
	for _, candidate := range wave {
		if candidate.Object.GetDeletionTimestamp() != nil {
			continue
		}

		result, err := applier.DeleteOrphan(dcx.Ctx, candidate)
		if err != nil {
			dcx.Mark.ResourcesUnderDeletion("deletion blocked: %v", err)
			return err
		}
		conflict = conflict || result.Conflict
	}

	if conflict {
		err := fmt.Errorf("deletion encountered UID conflicts; retrying")
		dcx.Mark.ResourcesUnderDeletion("deletion blocked: %v", err)
		return dcx.delayedRequeue(err)
	}
	return dcx.delayedRequeue(fmt.Errorf("deleting apply-order wave %d", highest))
}

// discoverDeletionInventory reconstructs the deletion search scope solely from
// the parent ApplySet metadata. It does not evaluate the current graph or CEL.
func (c *Controller) discoverDeletionInventory(
	dcx *DeletionContext,
) ([]applyset.OrphanCandidate, *applyset.ApplySet, error) {
	if err := applyset.ValidateParentInventory(dcx.Instance); err != nil {
		return nil, nil, fmt.Errorf("validate deletion inventory: %w", err)
	}
	applier := applyset.New(applyset.Config{
		Client:          dcx.Client,
		RESTMapper:      dcx.RestMapper,
		Log:             dcx.Log,
		ParentNamespace: dcx.Instance.GetNamespace(),
	}, dcx.Instance)
	inventory, err := applier.Project(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("project deletion inventory: %w", err)
	}
	candidates, err := applier.ListOrphans(dcx.Ctx, applyset.PruneOptions{
		KeepUIDs: sets.New[types.UID](),
		Scope:    inventory.PruneScope(),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list deletion inventory: %w", err)
	}
	return candidates, applier, nil
}

const fallbackDeletionOrder = 0

// highestDeletionWave returns only candidates in the highest remaining order.
// Resources without a valid positive order share fallbackDeletionOrder (wave 0),
// which runs after every ordered wave has disappeared. Note: children created
// by the classic engine (or prior to apply-order annotation rollout) carry no
// annotation and all land in fallback deletion wave 0; ordered deletion silently
// degrades to unordered for them until they are rewritten.
func highestDeletionWave(candidates []applyset.OrphanCandidate) ([]applyset.OrphanCandidate, int) {
	highest := fallbackDeletionOrder
	wave := make([]applyset.OrphanCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		raw := candidate.Object.GetAnnotations()[metadata.ApplyOrderAnnotation]
		order, err := strconv.Atoi(raw)
		if err != nil || order <= 0 {
			order = fallbackDeletionOrder
		}
		if order > highest {
			highest = order
			wave = wave[:0]
		}
		if order == highest {
			wave = append(wave, candidate)
		}
	}
	return wave, highest
}

// removeFinalizer clears managed state on the instance after deletions complete.
func (c *Controller) removeFinalizer(dcx *DeletionContext) error {
	// Release every recorded patch contribution so the field managers
	// relinquish their fields. Targets survive — patches never own them.
	contribs, err := controllergraph.ReadContributions(dcx.Instance)
	if err != nil {
		dcx.Mark.ResourcesUnderDeletion("deletion blocked: %v", err)
		return fmt.Errorf("read patch contributions on delete: %w", err)
	}
	if len(contribs) > 0 {
		if c.graphEngineExecutor != nil {
			if err := c.graphEngineExecutor.Release(dcx.Ctx, contribs); err != nil {
				dcx.Mark.ResourcesUnderDeletion("deletion blocked: %v", err)
				return fmt.Errorf("executor release: %w", err)
			}
		}
	}

	// Clean up coordinator watch requests before removing the finalizer.
	c.coordinator.RemoveInstance(c.gvr, types.NamespacedName{
		Name:      dcx.Instance.GetName(),
		Namespace: dcx.Instance.GetNamespace(),
	})

	patched, err := c.setUnmanaged(dcx, dcx.Instance)
	if err != nil {
		dcx.Mark.InstanceNotManaged("failed removing finalizer: %v", err)
		return err
	}
	if patched != nil {
		dcx.rebindInstance(patched)
	}
	dcx.Mark.ResourcesUnderDeletion("deleting resources")
	return nil
}

// setUnmanaged removes the instance finalizer using JSON merge patch with retry on conflict.
// Uses merge patch (not SSA) to avoid field manager ownership blocking finalizer removal.
// Returns the server's response, or nil when the finalizer was already absent
// and no request was made (callers must only rebind on a non-nil return).
func (c *Controller) setUnmanaged(dcx *DeletionContext, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if exist := metadata.HasInstanceFinalizer(obj); !exist {
		return nil, nil
	}
	dcx.Log.Info("Removing managed state", "name", obj.GetName(), "namespace", obj.GetNamespace())

	var updated *unstructured.Unstructured
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Re-fetch fresh object on each retry attempt
		current, err := dcx.InstanceClient().Get(dcx.Ctx, obj.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}

		// Check if finalizer still exists after re-fetch
		if !metadata.HasInstanceFinalizer(current) {
			updated = current
			return nil
		}

		clone := current.DeepCopy()
		metadata.RemoveInstanceFinalizer(clone)

		patchData, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{
				"resourceVersion": current.GetResourceVersion(),
				"finalizers":      clone.GetFinalizers(),
			},
		})
		if err != nil {
			return fmt.Errorf("failed to marshal finalizer patch: %w", err)
		}

		updated, err = dcx.InstanceClient().Patch(
			dcx.Ctx,
			current.GetName(),
			types.MergePatchType,
			patchData,
			metav1.PatchOptions{},
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update unmanaged state: %w", err)
	}
	return updated, nil
}
