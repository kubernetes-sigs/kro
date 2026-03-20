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

package instance

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/pkg/runtime"
)

// processExternalRefNode reads an external ref object and updates node state.
// Returns the resulting NodeState; the caller registers it.
func (c *Controller) processExternalRefNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	desiredList []*unstructured.Unstructured,
) (NodeState, error) {
	id := node.Spec.Meta.ID
	if len(desiredList) == 0 {
		return skippedState(), nil
	}
	desired := desiredList[0]

	// Register watch BEFORE reading the external resource.
	requestWatch(rcx, id, node.Spec.Meta.GVR, desired.GetName(), desired.GetNamespace())

	// External refs are read-only: fetch and push into runtime for dependency/readiness.
	ri := resourceClientFor(rcx, node.Spec.Meta, desired.GetNamespace())
	actual, err := ri.Get(rcx.Ctx, desired.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return waitingForReadinessState(fmt.Errorf("waiting for external reference %q: %w", id, err)), nil
		}
		fetchErr := fmt.Errorf("external ref get %s %s/%s: %w",
			desired.GroupVersionKind().String(), desired.GetNamespace(), desired.GetName(), err)
		return errorState(fetchErr), fetchErr
	}

	rcx.Log.V(2).Info("External reference resolved",
		"id", id,
		"gvk", desired.GroupVersionKind().String(),
		"namespace", actual.GetNamespace(),
		"name", actual.GetName(),
	)

	node.SetObserved([]*unstructured.Unstructured{actual})

	if err := node.CheckReadiness(); err != nil {
		if errors.Is(err, runtime.ErrWaitingForReadiness) {
			return waitingForReadinessState(fmt.Errorf("waiting for external reference %q: %w", id, err)), nil
		}
		return errorState(err), err
	}
	return readyState(), nil
}

// processExternalCollectionNode reads external resources matching a label selector
// and returns node state. The selector is extracted from the resolved template
// (desired), which was resolved by the standard template pipeline.
func (c *Controller) processExternalCollectionNode(
	rcx *ReconcileContext,
	node *runtime.Node,
	desired []*unstructured.Unstructured,
) (NodeState, error) {
	id := node.Spec.Meta.ID
	nodeMeta := node.Spec.Meta

	if len(desired) == 0 {
		return skippedState(), nil
	}

	// Extract the resolved selector from the template and convert to labels.Selector.
	// A missing selector means "select everything" (unfiltered list).
	var selector labels.Selector
	selectorRaw, found, err := unstructured.NestedMap(desired[0].Object, "metadata", "selector")
	if err != nil || !found {
		selector = labels.Everything()
	} else {
		ls := &metav1.LabelSelector{}
		if err := apimachineryruntime.DefaultUnstructuredConverter.FromUnstructured(selectorRaw, ls); err != nil {
			convErr := fmt.Errorf("failed to convert selector for %s: %w", id, err)
			return errorState(convErr), convErr
		}
		selector, err = metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			selErr := fmt.Errorf("invalid label selector for %s: %w", id, err)
			return errorState(selErr), selErr
		}
	}

	// Get namespace from the resolved template. For namespaced resources, an
	// empty namespace means "list across all namespaces" rather than falling
	// back to the instance namespace.
	ns := desired[0].GetNamespace()
	if !nodeMeta.Namespaced {
		ns = ""
	}

	// Register collection watch with the coordinator.
	requestCollectionWatch(rcx, id, nodeMeta.GVR, ns, selector)

	// LIST external resources matching the selector.
	var list *unstructured.UnstructuredList
	if ns != "" {
		list, err = rcx.Client.Resource(nodeMeta.GVR).Namespace(ns).List(rcx.Ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
	} else {
		list, err = rcx.Client.Resource(nodeMeta.GVR).List(rcx.Ctx, metav1.ListOptions{
			LabelSelector: selector.String(),
		})
	}
	if err != nil {
		listErr := fmt.Errorf("failed to list external collection %s: %w", id, err)
		return errorState(listErr), listErr
	}

	items := make([]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		items[i] = &list.Items[i]
	}

	node.SetObserved(items)

	if err := node.CheckReadiness(); err != nil {
		if errors.Is(err, runtime.ErrWaitingForReadiness) {
			return waitingForReadinessState(fmt.Errorf("waiting for external collection %q: %w", id, err)), nil
		}
		return errorState(err), err
	}

	rcx.Log.V(2).Info("External collection resolved",
		"id", id,
		"gvr", nodeMeta.GVR.String(),
		"count", len(items),
	)
	return readyState(), nil
}
