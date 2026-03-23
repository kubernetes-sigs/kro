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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// emitConditionEvents fires a K8s Event for every status condition that
// transitioned between the initial and final snapshots.
func emitConditionEvents(
	recorder record.EventRecorder,
	inst *unstructured.Unstructured,
	initial, final []v1alpha1.Condition,
) {
	initialByType := indexConditionsByType(initial)

	for _, cond := range final {
		old, existed := initialByType[cond.Type]
		if existed && old.Status == cond.Status {
			continue
		}

		// default shown in transition events when a condition appears for the first time (e.g. "none -> True")
		oldStatus := "none"
		if existed {
			oldStatus = string(old.Status)
		}

		reason := ""
		if cond.Reason != nil {
			reason = *cond.Reason
		}

		eventType := eventTypeForTransition(cond.Status)

		recorder.Eventf(inst, eventType, string(cond.Type),
			"%s -> %s: %s",
			oldStatus, cond.Status, reason,
		)
	}
}

func eventTypeForTransition(newStatus metav1.ConditionStatus) string {
	if newStatus == metav1.ConditionTrue {
		return corev1.EventTypeNormal
	}
	return corev1.EventTypeWarning
}

// conditionsFromInstance extracts status conditions from an unstructured object.
//
// TODO(perf): This performs a JSON marshal/unmarshal round-trip via
// unstructuredWrapper.GetConditions(). For better performance we could
// keep raw []v1alpha1.Condition at the resource level to avoid the
// conversion on every reconciliation.
func conditionsFromInstance(inst *unstructured.Unstructured) []v1alpha1.Condition {
	return (&unstructuredWrapper{inst}).GetConditions()
}

// indexConditionsByType builds a lookup map from condition type to condition.
func indexConditionsByType(conditions []v1alpha1.Condition) map[v1alpha1.ConditionType]v1alpha1.Condition {
	m := make(map[v1alpha1.ConditionType]v1alpha1.Condition, len(conditions))
	for _, c := range conditions {
		m[c.Type] = c
	}
	return m
}
