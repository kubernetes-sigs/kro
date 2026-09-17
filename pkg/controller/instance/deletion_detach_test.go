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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/instance/applyset"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

// asDetached stamps the deletion-policy annotation the adapter puts on a
// managed resource declared deletionPolicy: Detach.
func asDetached(obj *unstructured.Unstructured) *unstructured.Unstructured {
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[metadata.DeletionPolicyAnnotation] = string(v1alpha1.DeletionPolicyDetach)
	obj.SetAnnotations(annotations)
	return obj
}

func deployNodeNamed(id string) *graph.Node {
	return &graph.Node{Meta: graph.NodeMeta{
		ID:         id,
		Type:       graph.NodeTypeResource,
		GVR:        controllerTestDeployGVR,
		Namespaced: true,
	}}
}

// A Detach resource is released instead of deleted, and releasing it has to
// clear the instance's finalizer in the same pass: the resource is never going
// away, so an implementation that left it in the candidate set would requeue
// forever and the instance would never finish deleting.
func TestReconcileDeletionReleasesDetachedAndFinishes(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)

	managed := asDetached(newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1))

	controller, dcx, raw := newControllerAndDeletionContext(
		t, instance, newTestGraph(deployNodeNamed("deploy")), managed)

	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})

	require.NoError(t, controller.reconcileDeletion(dcx))
	assert.Empty(t, deleted, "a Detach resource must never be deleted")
	assert.False(t, metadata.HasInstanceFinalizer(dcx.Instance))

	live, err := raw.Tracker().Get(controllerTestDeployGVR, "default", "demo")
	require.NoError(t, err, "the released resource must survive the instance")

	released, ok := live.(*unstructured.Unstructured)
	require.True(t, ok)
	assert.NotContains(t, released.GetLabels(), applyset.ApplysetPartOfLabel)
	assert.NotContains(t, released.GetLabels(), metadata.InstanceIDLabel)
	assert.NotContains(t, released.GetAnnotations(), metadata.DeletionPolicyAnnotation)
	assert.NotContains(t, released.GetAnnotations(), metadata.ApplyOrderAnnotation)
}

// Detached resources leave the candidate set before the wave arithmetic, so a
// retained resource in a later wave cannot stall the deletion of the resources
// behind it.
func TestReconcileDeletionDetachDoesNotBlockLowerWaves(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)

	keep := asDetached(newManagedObject(newDeploymentObject("keep", "default"), instance, "keep", 2))
	drop := newManagedObject(newDeploymentObject("drop", "default"), instance, "drop", 1)

	controller, dcx, raw := newControllerAndDeletionContext(
		t, instance, newTestGraph(deployNodeNamed("keep"), deployNodeNamed("drop")), keep, drop)

	var deleted []string
	raw.PrependReactor("delete", "deployments", func(action k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		deleted = append(deleted, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})

	err := controller.reconcileDeletion(dcx)
	var retryAfter *requeue.RequeueNeededAfter
	require.ErrorAs(t, err, &retryAfter, "deletion of the remaining wave requeues")
	assert.Equal(t, []string{"drop"}, deleted)

	_, getErr := raw.Tracker().Get(controllerTestDeployGVR, "default", "keep")
	require.NoError(t, getErr)
}

// A failed release keeps the finalizer: dropping it would leave an object
// behind still labelled as a member of an instance that no longer exists, which
// no later reconcile can clean up.
func TestReconcileDeletionReleaseFailureRetainsFinalizer(t *testing.T) {
	instance := newInstanceObject("demo", "default")
	metadata.SetInstanceFinalizer(instance)

	managed := asDetached(newManagedObject(newDeploymentObject("demo", "default"), instance, "deploy", 1))

	controller, dcx, raw := newControllerAndDeletionContext(
		t, instance, newTestGraph(deployNodeNamed("deploy")), managed)
	raw.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, apimachineryruntime.Object, error) {
		return true, nil, errors.New("update rejected")
	})

	err := controller.reconcileDeletion(dcx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "release detached resource")
	assert.True(t, metadata.HasInstanceFinalizer(dcx.Instance))
}
