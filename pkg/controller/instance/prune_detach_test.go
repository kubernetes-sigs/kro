// Copyright 2026 The Kubernetes Authors.
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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph/revisions"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// Prune is the other way a resource leaves an instance: its node was removed
// from the graph, its includeWhen turned false, or a forEach shrank. It must
// honour the policy too, and the policy can only come off the live object
// because the current graph no longer describes the resource at all.
func TestPruneGraphEngineOrphans_HonoursDeletionPolicy(t *testing.T) {
	comp := newTestRealCompiler(t)

	inst := newInstanceObject("demo", "default")
	addDeletionScope(inst, controllerTestDeployGVK, "default")

	keep := asDetached(newManagedObject(newDeploymentObject("keep", "default"), inst, "keep", 1))
	keep.SetUID(types.UID("keep-uid"))
	drop := newManagedObject(newDeploymentObject("drop", "default"), inst, "drop", 1)
	drop.SetUID(types.UID("drop-uid"))

	raw := newControllerTestDynamicClient(t, inst.DeepCopy(), keep, drop)
	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return false, nil, nil
	})

	c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
	applier := applyset.New(applyset.Config{
		Client:          clientSet.Dynamic(),
		RESTMapper:      clientSet.RESTMapper(),
		Log:             c.log,
		ParentNamespace: inst.GetNamespace(),
	}, inst)

	// Nothing is applied this cycle, so both resources are prune candidates.
	supersetMeta, err := applier.Project(nil)
	require.NoError(t, err)

	pruned, conflictFree, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, nil, supersetMeta)
	require.NoError(t, err)
	assert.True(t, pruned)
	assert.True(t, conflictFree)
	assert.Equal(t, []string{"drop"}, deleted, "only the Delete-policy resource may be deleted")

	stored, err := raw.Tracker().Get(controllerTestDeployGVR, "default", "keep")
	require.NoError(t, err)
	released, ok := stored.(*unstructured.Unstructured)
	require.True(t, ok)
	assert.NotContains(t, released.GetLabels(), applyset.ApplysetPartOfLabel,
		"a released resource must stop being an ApplySet member or prune finds it again every cycle")
	assert.NotContains(t, released.GetAnnotations(), metadata.DeletionPolicyAnnotation)

	// The released resource must not come back as a candidate.
	remaining, err := applier.ListOrphans(context.Background(), applyset.PruneOptions{
		Scope: supersetMeta.PruneScope(),
	})
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestPruneGraphEngineOrphans_ReleaseErrorIsReturned(t *testing.T) {
	comp := newTestRealCompiler(t)

	inst := newInstanceObject("demo", "default")
	addDeletionScope(inst, controllerTestDeployGVK, "default")
	keep := asDetached(newManagedObject(newDeploymentObject("keep", "default"), inst, "keep", 1))

	raw := newControllerTestDynamicClient(t, inst.DeepCopy(), keep)
	raw.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, assert.AnError
	})

	c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
	applier := applyset.New(applyset.Config{
		Client:          clientSet.Dynamic(),
		RESTMapper:      clientSet.RESTMapper(),
		Log:             c.log,
		ParentNamespace: inst.GetNamespace(),
	}, inst)

	supersetMeta, err := applier.Project(nil)
	require.NoError(t, err)

	_, conflictFree, err := c.pruneGraphEngineOrphans(context.Background(), c.log, applier, nil, supersetMeta)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release orphan")
	assert.False(t, conflictFree, "a failed release must not let the inventory shrink")
}

// The policy travels on the object, so a resource written before the feature
// existed, or one whose annotation says something unexpected, keeps the
// historical delete behaviour rather than silently becoming undeletable.
func TestPruneGraphEngineOrphans_UnknownPolicyStillDeletes(t *testing.T) {
	comp := newTestRealCompiler(t)

	inst := newInstanceObject("demo", "default")
	addDeletionScope(inst, controllerTestDeployGVK, "default")

	drop := newManagedObject(newDeploymentObject("drop", "default"), inst, "drop", 1)
	annotations := drop.GetAnnotations()
	annotations[metadata.DeletionPolicyAnnotation] = "detach" // wrong case
	drop.SetAnnotations(annotations)

	raw := newControllerTestDynamicClient(t, inst.DeepCopy(), drop)
	c, clientSet := newGraphEngineControllerUnderTest(t, raw, testEmptyRGDSpec(), revisions.RevisionStateActive, comp, nil)
	applier := applyset.New(applyset.Config{
		Client:          clientSet.Dynamic(),
		RESTMapper:      clientSet.RESTMapper(),
		Log:             c.log,
		ParentNamespace: inst.GetNamespace(),
	}, inst)

	supersetMeta, err := applier.Project(nil)
	require.NoError(t, err)

	_, _, err = c.pruneGraphEngineOrphans(context.Background(), c.log, applier, nil, supersetMeta)
	require.NoError(t, err)

	_, err = raw.Tracker().Get(controllerTestDeployGVR, "default", "drop")
	assert.Error(t, err)
	assert.Equal(t, v1alpha1.DeletionPolicyDelete, metadata.DeletionPolicyOf(drop))
}
