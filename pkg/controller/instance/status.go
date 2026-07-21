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
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
	"github.com/kubernetes-sigs/kro/pkg/requeue"
)

const (
	Ready           = string(v1alpha1.InstanceConditionTypeReady)
	InstanceManaged = string(v1alpha1.InstanceConditionTypeInstanceManaged)
	GraphResolved   = string(v1alpha1.InstanceConditionTypeGraphResolved)
	ResourcesReady  = string(v1alpha1.InstanceConditionTypeResourcesReady)
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
	return decodeConditions(condSlice)
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

// decodeConditions converts a slice of condition maps into typed conditions,
// skipping malformed entries.
func decodeConditions(raw []interface{}) []v1alpha1.Condition {
	result := make([]v1alpha1.Condition, 0, len(raw))
	for _, item := range raw {
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

// updateConditionsStatus persists conditions and state from the instance
// object. Used on early-exit paths (e.g. graph resolution failure) where a
// full ReconcileContext is not available. When the RGD declares author
// conditions this path cannot evaluate them (there is no graph), so only
// `state` is written and the persisted conditions are left untouched.
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
		if !c.reconcileConfig.HasAuthorConditions {
			if conds, found, _ := unstructured.NestedSlice(inst.Object, "status", "conditions"); found {
				status["conditions"] = conds
			}
		}
		status["state"] = string(v1alpha1.InstanceStateError)
		cur.Object["status"] = status
		// Mirror persisted status onto the instance so the deferred
		// emitters read what's on the wire, not the marker's built-ins.
		inst.Object["status"] = status
		if c.namespaced {
			_, err = ri.Namespace(inst.GetNamespace()).UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		} else {
			_, err = ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		}
		return err
	})
}

func (c *Controller) updateStatus(rcx *ReconcileContext) error {
	previousState, _ := rcx.WireStatus["state"].(string)

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

	// When the RGD declares author conditions, only those appear on the
	// wire. kro's built-ins stay readable from author CEL through
	// runtime.condition(schema, 'X').
	instanceNode := rcx.Runtime.Instance()
	if instanceNode.HasConditions() {
		authored, incomplete, evalErr := instanceNode.EvaluateConditions(rcx.Log, builtinConditions(rcx.Instance))

		// Read previous conditions from the wire snapshot, not rcx.Instance:
		// the markers have already overwritten any built-in-typed override.
		conds, _ := rcx.WireStatus["conditions"].([]interface{})
		previous := decodeConditions(conds)
		stamped := stampAuthorConditions(authored, previous, rcx.Instance.GetGeneration())
		if incomplete {
			// Keep the previously persisted conditions for the types that
			// produced no output this reconcile.
			stamped = mergeWithPrevious(stamped, previous)
		}
		status["conditions"] = conditionsToInterfaceSlice(stamped)

		// A degraded result still surfaces its surviving conditions; set
		// state=Error rather than failing the reconcile.
		if evalErr != nil {
			rcx.Log.Error(evalErr, "author conditions degraded; setting state=Error")
			status["state"] = string(v1alpha1.InstanceStateError)
		}
	}

	// Mirror the computed status onto the instance so the deferred emitters
	// read what's on the wire, not the marker's built-ins.
	rcx.Instance.Object["status"] = status

	// Skip the API server write if the persisted status already matches.
	// This prevents an infinite reconcile loop where every unconditional
	// UpdateStatus bumps resourceVersion, which triggers a watch event,
	// which re-enqueues the instance, which reconciles again.
	if statusesMatch(rcx.WireStatus, status) {
		return nil
	}

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := rcx.InstanceClient().Get(rcx.Ctx, rcx.Instance.GetName(), metav1.GetOptions{})
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

	// Record the transition using the state actually written, which may
	// differ from the state manager's view (degraded author conditions).
	writtenState, _ := status["state"].(string)
	if previousState != writtenState {
		gvk := rcx.Instance.GroupVersionKind().String()
		metrics.InstanceStateTransitionsTotal.WithLabelValues(
			gvk,
			previousState,
			writtenState,
		).Inc()
	}

	return nil
}

// statusesMatch reports whether the computed status would persist the same
// JSON the wire snapshot already holds. The canonical JSON encodings are
// compared instead of the Go values because the two sides type numbers
// differently: apimachinery decodes a persisted whole number as int64,
// while CEL evaluation yields float64 (a whole 3.0 included), so a
// DeepEqual would keep reporting a difference that serializes identically.
// Marshal errors fall through to a write, which the API server no-ops.
func statusesMatch(wire, computed map[string]interface{}) bool {
	wireJSON, err := json.Marshal(wire)
	if err != nil {
		return false
	}
	computedJSON, err := json.Marshal(computed)
	if err != nil {
		return false
	}
	return bytes.Equal(wireJSON, computedJSON)
}

// builtinConditions returns kro's built-in conditions as computed for this
// reconcile on the (marker-mutated) instance object.
func builtinConditions(inst *unstructured.Unstructured) []v1alpha1.Condition {
	all := condSet.For(&unstructuredWrapper{inst}).List()
	out := make([]v1alpha1.Condition, 0, len(v1alpha1.KROBuiltinConditionTypes))
	for _, c := range all {
		if _, ok := v1alpha1.KROBuiltinConditionTypes[string(c.Type)]; ok {
			out = append(out, c)
		}
	}
	return out
}

// mergeWithPrevious appends previously persisted conditions whose types were
// not produced by the current evaluation, so conditions do not disappear
// from the wire while their expression cannot produce output.
func mergeWithPrevious(current, previous []v1alpha1.Condition) []v1alpha1.Condition {
	have := make(map[v1alpha1.ConditionType]struct{}, len(current))
	for _, c := range current {
		have[c.Type] = struct{}{}
	}
	for _, p := range previous {
		if _, ok := have[p.Type]; !ok {
			current = append(current, p)
		}
	}
	return current
}

func (rcx *ReconcileContext) initialStatus() map[string]interface{} {
	cs := condSet.For(&unstructuredWrapper{rcx.Instance})

	// Start fresh - user-defined status fields come solely from current
	// resolution, so fields disappear when their dependencies become
	// unavailable. Only kro's built-in conditions are written; anything else
	// (e.g. author conditions left over after a conditions: block was
	// removed) is dropped. State is a plain string to compare equal to the
	// wire value.
	status := map[string]interface{}{
		"conditions": conditionsToInterfaceSlice(builtinConditions(rcx.Instance)),
	}
	if cs.IsRootReady() {
		status["state"] = string(v1alpha1.InstanceStateActive)
	} else {
		status["state"] = string(rcx.StateManager.State)
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

// stampAuthorConditions converts evaluated library.Condition values into
// wire-shaped v1alpha1.Condition values, preserving lastTransitionTime when
// the status hasn't changed and stamping observedGeneration with the
// instance's current generation.
func stampAuthorConditions(
	authored []library.Condition,
	previous []v1alpha1.Condition,
	generation int64,
) []v1alpha1.Condition {
	prevByType := make(map[string]v1alpha1.Condition, len(previous))
	for _, p := range previous {
		prevByType[string(p.Type)] = p
	}

	out := make([]v1alpha1.Condition, 0, len(authored))
	now := metav1.Now()

	for _, a := range authored {
		cond := v1alpha1.Condition{
			Type:               v1alpha1.ConditionType(a.ConditionType),
			Status:             metav1.ConditionStatus(a.Status),
			Reason:             stringPtr(a.Reason),
			Message:            stringPtr(a.Message),
			ObservedGeneration: generation,
		}

		if prev, ok := prevByType[a.ConditionType]; ok && string(prev.Status) == a.Status && prev.LastTransitionTime != nil {
			cond.LastTransitionTime = prev.LastTransitionTime
		} else {
			t := now
			cond.LastTransitionTime = &t
		}
		out = append(out, cond)
	}
	return out
}

// conditionsToInterfaceSlice converts a typed Condition slice into the
// []interface{} shape expected by unstructured status writes.
func conditionsToInterfaceSlice(conds []v1alpha1.Condition) []interface{} {
	out := make([]interface{}, 0, len(conds))
	for _, c := range conds {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// stringPtr returns a pointer to s, or nil if s is empty. The wire shape
// represents reason and message as omitempty pointers.
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
