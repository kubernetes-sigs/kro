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

// Inspired by https://github.com/knative/pkg/tree/97c7258e3a98b81459936bc7a29dc6a9540fa357/apis,
// but we chose to diverge due to the unacceptably large dependency closure of knative/pkg.

package apis

import (
	"fmt"
	"reflect"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/kro-run/kro/api/v1alpha1"
)

// ConditionSet provides methods for evaluating Conditions.
// +k8s:deepcopy-gen=false
type ConditionSet struct {
	ConditionTypes
	object Object
}

// Root returns the root Condition, typically "Ready" or "Succeeded"
func (c ConditionSet) Root() *v1alpha1.Condition {
	if c.object == nil {
		return nil
	}
	return c.Get(c.root)
}

// IsRootReady returns the readiness of the root Condition.
func (c ConditionSet) IsRootReady() bool {
	if c.object == nil {
		return false
	}
	root := c.Get(c.root)
	if root.IsTrue() && root.ObservedGeneration == c.object.GetGeneration() {
		return true
	}
	return false
}

func (c ConditionSet) List() []v1alpha1.Condition {
	if c.object == nil {
		return nil
	}
	return c.object.GetConditions()
}

// Get finds and returns the Condition that matches the ConditionType
// previously set on Conditions.
func (c ConditionSet) Get(t string) *v1alpha1.Condition {
	if c.object == nil {
		return nil
	}
	for _, c := range c.object.GetConditions() {
		if string(c.Type) == t {
			return &c
		}
	}
	return nil
}

// IsTrue returns true if all condition types are true.
func (c ConditionSet) IsTrue(conditionTypes ...string) bool {
	for _, conditionType := range conditionTypes {
		if !c.Get(conditionType).IsTrue() {
			return false
		}
	}
	return true
}

// IsDependentCondition returns true if the provided condition is involved in calculating the root condition.
func (c ConditionSet) IsDependentCondition(t string) bool {
	for _, cond := range c.dependents {
		if cond == t {
			return true
		}
	}
	return t == c.root
}

// Set sets or updates the Condition on Conditions for Condition.Type.
// If there is an update, Conditions are stored back sorted.
func (c ConditionSet) Set(condition v1alpha1.Condition) (modified bool) {
	if c.object == nil {
		return false
	}

	var conditions []v1alpha1.Condition
	var foundCondition bool

	condition.ObservedGeneration = c.object.GetGeneration()
	for _, cond := range c.object.GetConditions() {
		if cond.Type != condition.Type {
			// If we are deleting, we just bump all the observed generations
			if !c.object.GetDeletionTimestamp().IsZero() {
				cond.ObservedGeneration = c.object.GetGeneration()
			}
			conditions = append(conditions, cond)
		} else {
			foundCondition = true
			if condition.Status == cond.Status {
				condition.LastTransitionTime = cond.LastTransitionTime
			} else {
				condition.LastTransitionTime = ptr.To(metav1.Now())
			}
			if reflect.DeepEqual(condition, cond) {
				return false
			}
		}
	}
	if !foundCondition {
		// Dependent conditions should always be set, so if it's not found, that means
		// that we are initializing the condition type, and it's last "transition" was object creation
		if c.IsDependentCondition(condition.Type.String()) {
			condition.LastTransitionTime = ptr.To(c.object.GetCreationTimestamp())
		} else {
			condition.LastTransitionTime = ptr.To(metav1.Now())
		}
	}
	conditions = append(conditions, condition)
	// Sorted for convenience of the consumer, i.e. kubectl.
	sort.SliceStable(conditions, func(i, j int) bool {
		// Order the root status condition at the end
		if conditions[i].Type.String() == c.root || conditions[j].Type.String() == c.root {
			return conditions[j].Type.String() == c.root
		}

		// Handle nil LastTransitionTime
		if conditions[i].LastTransitionTime == nil && conditions[j].LastTransitionTime == nil {
			return false // Equal, order doesn't matter
		}
		if conditions[i].LastTransitionTime == nil {
			return true // nil comes before non-nil
		}
		if conditions[j].LastTransitionTime == nil {
			return false // non-nil comes after nil
		}

		return conditions[i].LastTransitionTime.Time.Before(conditions[j].LastTransitionTime.Time)
	})
	c.object.SetConditions(conditions)

	// Recompute the root condition after setting any other condition
	c.recomputeRootCondition(condition.Type.String())
	return true
}

// Clear removes the independent condition that matches the ConditionType
// Not implemented for dependent conditions
func (c ConditionSet) Clear(t string) error {
	if c.object == nil {
		return nil
	}

	var conditions []v1alpha1.Condition

	// Dependent conditions are not handled as they can't be nil
	if c.IsDependentCondition(t) {
		return fmt.Errorf("clearing dependent conditions not implemented")
	}
	cond := c.Get(t)
	if cond == nil {
		return nil
	}
	for _, c := range c.object.GetConditions() {
		if c.Type.String() != t {
			conditions = append(conditions, c)
		}
	}

	// Sorted for convenience of the consumer, i.e. kubectl.
	sort.Slice(conditions, func(i, j int) bool { return conditions[i].Type < conditions[j].Type })
	c.object.SetConditions(conditions)

	return nil
}

// SetTrue sets the status of conditionType to true with the reason, and then marks the root condition to
// true if all other dependents are also true.
func (c ConditionSet) SetTrue(conditionType string) (modified bool) {
	return c.SetTrueWithReason(conditionType, conditionType, "")
}

// SetTrueWithReason sets the status of conditionType to true with the reason, and then marks the root condition to
// true if all other dependents are also true.
func (c ConditionSet) SetTrueWithReason(conditionType string, reason, message string) (modified bool) {
	return c.Set(v1alpha1.Condition{
		Type:    v1alpha1.ConditionType(conditionType),
		Status:  metav1.ConditionTrue,
		Reason:  ptr.To(reason),
		Message: ptr.To(message),
	})
}

// SetUnknown sets the status of conditionType to Unknown and also sets the root condition
// to Unknown if no other dependent condition is in an error state.
func (c ConditionSet) SetUnknown(conditionType string) (modified bool) {
	return c.SetUnknownWithReason(conditionType, "AwaitingReconciliation",
		fmt.Sprintf("condition %q is awaiting reconciliation", conditionType))
}

// SetUnknownWithReason sets the status of conditionType to Unknown with the reason, and also sets the root condition
// to Unknown if no other dependent condition is in an error state.
func (c ConditionSet) SetUnknownWithReason(conditionType string, reason, message string) (modified bool) {
	return c.Set(v1alpha1.Condition{
		Type:    v1alpha1.ConditionType(conditionType),
		Status:  metav1.ConditionUnknown,
		Reason:  ptr.To(reason),
		Message: ptr.To(message),
	})
}

// SetFalse sets the status of conditionType and the root condition to False.
func (c ConditionSet) SetFalse(conditionType string, reason, message string) (modified bool) {
	return c.Set(v1alpha1.Condition{
		Type:    v1alpha1.ConditionType(conditionType),
		Status:  metav1.ConditionFalse,
		Reason:  ptr.To(reason),
		Message: ptr.To(message),
	})
}

// recomputeRootCondition marks the root condition to true if all other dependents are also true.
func (c ConditionSet) recomputeRootCondition(conditionType string) {
	if conditionType == c.root {
		return
	}
	if conditions := c.findUnhealthyDependents(); len(conditions) == 0 {
		c.SetTrue(c.root)
	} else if unhealthy, found := findMostUnhealthy(conditions); found {
		c.Set(v1alpha1.Condition{
			Type:    v1alpha1.ConditionType(c.root),
			Status:  unhealthy.Status,
			Reason:  unhealthy.Reason,
			Message: unhealthy.Message,
		})
	}
}

func findMostUnhealthy(deps []v1alpha1.Condition) (v1alpha1.Condition, bool) {
	// Sort set conditions by time.
	sort.Slice(deps, func(i, j int) bool {
		// Handle nil LastTransitionTime
		if deps[i].LastTransitionTime == nil && deps[j].LastTransitionTime == nil {
			return false // Equal, order doesn't matter
		}
		if deps[i].LastTransitionTime == nil {
			return false // nil comes after non-nil (opposite of Before)
		}
		if deps[j].LastTransitionTime == nil {
			return true // non-nil comes before nil (opposite of Before)
		}

		return deps[i].LastTransitionTime.Time.After(deps[j].LastTransitionTime.Time)
	})

	// First check the conditions with Status == False.
	for _, c := range deps {
		// False conditions trump Unknown.
		if c.IsFalse() {
			return c, true
		}
	}
	// Second check for conditions with Status == Unknown.
	for _, c := range deps {
		if c.IsUnknown() {
			return c, true
		}
	}

	// All dependents are fine.
	return v1alpha1.Condition{}, false
}

func (c ConditionSet) findUnhealthyDependents() []v1alpha1.Condition {
	if len(c.dependents) == 0 {
		return nil
	}
	deps := make([]v1alpha1.Condition, 0, len(c.object.GetConditions()))
	for _, dep := range c.object.GetConditions() {
		if c.DependsOn(dep.Type.String()) {
			if dep.IsFalse() || dep.IsUnknown() || dep.ObservedGeneration != c.object.GetGeneration() {
				deps = append(deps, dep)
			}
		}
	}

	// Sort set conditions by time.
	sort.Slice(deps, func(i, j int) bool {
		// Handle nil LastTransitionTime
		if deps[i].LastTransitionTime == nil && deps[j].LastTransitionTime == nil {
			return false // Equal, order doesn't matter
		}
		if deps[i].LastTransitionTime == nil {
			return false // nil comes after non-nil (opposite of Before)
		}
		if deps[j].LastTransitionTime == nil {
			return true // non-nil comes before nil (opposite of Before)
		}

		return deps[i].LastTransitionTime.Time.After(deps[j].LastTransitionTime.Time)
	})
	return deps
}
