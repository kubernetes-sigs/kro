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

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

const (
	Ready           = "Ready"
	InstanceManaged = "InstanceManaged"
	GraphResolved   = "GraphResolved"
	ResourcesReady  = "ResourcesReady"
)

var condSet = apis.NewReadyConditions(InstanceManaged, GraphResolved, ResourcesReady)

func NewConditionsMarkerFor(obj *unstructured.Unstructured) *ConditionsMarker {
	return &ConditionsMarker{
		cs: condSet.For(&unstructuredWrapper{obj}),
	}
}

type unstructuredWrapper struct {
	*unstructured.Unstructured
}

func (u *unstructuredWrapper) GetConditions() []v1alpha1.Condition {
	condSlice, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return []v1alpha1.Condition{}
	}
	result := make([]v1alpha1.Condition, 0, len(condSlice))
	for _, item := range condSlice {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var c v1alpha1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err != nil {
			continue
		}
		result = append(result, c)
	}
	return result
}

func (u *unstructuredWrapper) SetConditions(conditions []v1alpha1.Condition) {
	conditionsInterface := make([]interface{}, 0, len(conditions))
	for _, c := range conditions {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			return // Fail silently - could log this in the future
		}
		conditionsInterface = append(conditionsInterface, raw)
	}
	if err := unstructured.SetNestedSlice(u.Object, conditionsInterface, "status", "conditions"); err != nil {
		return // Fail silently - could log this in the future
	}
}

type ConditionsMarker struct {
	cs apis.ConditionSet
}

// InstanceManaged signals the instance has proper finalizers and labels set.
func (m *ConditionsMarker) InstanceManaged() {
	m.cs.SetTrueWithReason(InstanceManaged, "Managed", "instance is properly managed with finalizers and labels")
}

// InstanceNotManaged signals there was an issue setting up the instance management.
func (m *ConditionsMarker) InstanceNotManaged(format string, a ...any) {
	m.cs.SetFalse(InstanceManaged, "ManagementFailed", fmt.Sprintf(format, a...))
}

// GraphResolved signals the runtime graph has been created and resources resolved.
func (m *ConditionsMarker) GraphResolved() {
	m.cs.SetTrueWithReason(GraphResolved, "Resolved", "runtime graph created and all resources resolved")
}

// GraphResolutionFailed signals there was an issue creating the runtime graph or resolving resources.
func (m *ConditionsMarker) GraphResolutionFailed(msg string, args ...any) {
	m.cs.SetFalse(GraphResolved, "ResolutionFailed", fmt.Sprintf(msg, args...))
}

// ResourcesReady signals all resources in the graph are created and ready.
func (m *ConditionsMarker) ResourcesReady() {
	m.cs.SetTrueWithReason(ResourcesReady, "AllResourcesReady", "all resources are created and ready")
}

// ResourcesNotReady signals there are resources in the graph that are not ready.
func (m *ConditionsMarker) ResourcesNotReady(msg string, args ...any) {
	m.cs.SetFalse(ResourcesReady, "NotReady", fmt.Sprintf(msg, args...))
}

// ResourcesDeleting signals there are managed resources currently terminating.
func (m *ConditionsMarker) ResourcesDeleting(msg string, args ...any) {
	m.cs.SetFalse(ResourcesReady, "ResourceDeleting", fmt.Sprintf(msg, args...))
}

// ResourcesUnderDeletion signals the controller is currently deleting resources.
func (m *ConditionsMarker) ResourcesUnderDeletion(msg string, args ...any) {
	m.cs.SetUnknownWithReason(ResourcesReady, "UnderDeletion", fmt.Sprintf(msg, args...))
}

// updateConditionsStatus persists only the conditions and state from the
// instance object. Used on early-exit paths (e.g. graph resolution failure)
// where a full ReconcileContext is not available.
func (c *Controller) updateConditionsStatus(ctx context.Context, inst *unstructured.Unstructured) error {
	ri := c.client.Dynamic().Resource(c.gvr)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var cur *unstructured.Unstructured
		var err error
		if c.namespaced {
			cur, err = ri.Namespace(inst.GetNamespace()).Get(ctx, inst.GetName(), metav1.GetOptions{})
		} else {
			cur, err = ri.Get(ctx, inst.GetName(), metav1.GetOptions{})
		}
		if err != nil {
			return err
		}
		status, _, _ := unstructured.NestedMap(cur.Object, "status")
		if status == nil {
			status = map[string]interface{}{}
		}
		// Copy conditions from the marked instance.
		if conds, found, _ := unstructured.NestedSlice(inst.Object, "status", "conditions"); found {
			status["conditions"] = conds
		}
		status["state"] = string(v1alpha1.InstanceStateError)
		cur.Object["status"] = status
		if c.namespaced {
			_, err = ri.Namespace(inst.GetNamespace()).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		} else {
			_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		}
		return err
	})
}

func (c *Controller) updateStatus(rcx *ReconcileContext) error {
	previousState, _, _ := unstructured.NestedString(rcx.Instance.Object, "status", "state")

	rcx.updateInstanceState()
	status := rcx.initialStatus()

	// instance desired is guaranteed to have one item.
	desired, err := rcx.Runtime.Instance().GetDesired()
	if err != nil {
		return err
	}
	if resolved, found, _ := unstructured.NestedMap(desired[0].Object, "status"); found {
		for k, v := range resolved {
			if k == "conditions" || k == "state" {
				continue
			}
			status[k] = v
		}
	}

	// Skip the API server write if status hasn't changed. This prevents an
	// infinite reconcile loop where every unconditional UpdateStatus bumps
	// resourceVersion, which triggers a watch event, which re-enqueues the
	// instance, which reconciles again.
	oldStatus, _, _ := unstructured.NestedMap(rcx.Instance.Object, "status")
	if equality.Semantic.DeepEqual(oldStatus, status) {
		return nil
	}

	inst := rcx.Instance.DeepCopy()
	inst.Object["status"] = status

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := rcx.InstanceClient().Get(rcx.Ctx, inst.GetName(), metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Object["status"] = status
		_, err = rcx.InstanceClient().UpdateStatus(rcx.Ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return err
	}

	newState := rcx.StateManager.State
	if v1alpha1.InstanceState(previousState) != newState {
		gvk := rcx.Instance.GroupVersionKind().String()
		metrics.InstanceStateTransitionsTotal.WithLabelValues(
			gvk,
			previousState,
			string(newState),
		).Inc()
	}

	return nil
}

func (rcx *ReconcileContext) initialStatus() map[string]interface{} {
	inst := rcx.Instance

	cs := condSet.For(&unstructuredWrapper{inst})
	conds := cs.List()

	arr := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			panic(err)
		}
		arr = append(arr, raw)
	}

	// Start fresh - user-defined status fields come solely from current resolution,
	// not preserved from previous status. This ensures fields disappear when their
	// dependencies become unavailable.
	status := map[string]interface{}{
		"conditions": arr,
	}
	if cs.IsRootReady() {
		status["state"] = v1alpha1.InstanceStateActive
	} else {
		status["state"] = rcx.StateManager.State
	}
	return status
}

func (rcx *ReconcileContext) updateInstanceState() {
	switch rcx.StateManager.ReconcileErr.(type) {
	case *requeue.NoRequeue, *requeue.RequeueNeeded, *requeue.RequeueNeededAfter:
		return
	default:
		rcx.StateManager.Update()
	}
}
