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

package metrics

import (
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

// Condition-metric label keys.
const (
	labelGVR             = "gvr"
	labelNamespace       = "namespace"
	labelName            = "name"
	labelConditionType   = "condition_type"
	labelConditionStatus = "condition_status"
	labelReason          = "reason"
)

var (
	InstanceStateTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_state_transitions_total",
			Help: "Total number of instance state transitions per GVR",
		},
		[]string{"gvr", "from_state", "to_state"},
	)

	InstanceReconcileDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "instance_reconcile_duration_seconds",
			Help:    "Duration of instance reconciliation in seconds per GVR",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 15, 20, 25, 30, 45, 60, 120},
		},
		[]string{"gvr"},
	)

	InstanceReconcileTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_reconcile_total",
			Help: "Total number of instance reconciliations per GVR",
		},
		[]string{"gvr"},
	)

	InstanceReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_reconcile_errors_total",
			Help: "Total number of instance reconciliation errors per GVR",
		},
		[]string{"gvr"},
	)

	InstanceGraphResolutionSuccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_graph_resolution_success_total",
			Help: "Total number of successful graph resolutions during instance reconciliation",
		},
		[]string{"gvr"},
	)

	InstanceGraphResolutionFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_graph_resolution_failures_total",
			Help: "Total number of graph resolution failures during instance reconciliation",
		},
		[]string{"gvr", "reason"},
	)

	InstanceGraphResolutionPendingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "instance_graph_resolution_pending_total",
			Help: "Total number of graph resolutions deferred due to pending revision",
		},
		[]string{"gvr"},
	)

	// InstanceConditionCurrentStatusSeconds tracks the duration an instance condition
	// has been in its current state. The value is computed at reconcile time
	// as time.Since(lastTransitionTime). To get fresher gauge values, consider
	// lowering --dynamic-controller-default-resync-period.
	InstanceConditionCurrentStatusSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "instance_condition_current_status_seconds",
			Help: "The current amount of time in seconds that an instance status condition has been in a specific state.",
		},
		[]string{labelGVR, labelNamespace, labelName, labelConditionType, labelConditionStatus, labelReason},
	)
)

// emitConditionMetrics computes the diff between initial and final conditions,
// emits gauge metrics for all current conditions, emits structured logs for
// changed conditions, and cleans up gauges for disappeared conditions.
func EmitConditionMetrics(
	log logr.Logger,
	gvr schema.GroupVersionResource,
	inst *unstructured.Unstructured,
	initialConditions []v1alpha1.Condition,
	finalConditions []v1alpha1.Condition,
) {
	gvrKey := gvr.String()
	ns := inst.GetNamespace()
	name := inst.GetName()

	initialByType := indexConditionsByType(initialConditions)

	finalTypes := make(map[v1alpha1.ConditionType]struct{}, len(finalConditions))

	for _, cond := range finalConditions {
		finalTypes[cond.Type] = struct{}{}
		reason := ptr.Deref(cond.Reason, "")

		var durationSeconds float64
		if cond.LastTransitionTime != nil {
			durationSeconds = time.Since(cond.LastTransitionTime.Time).Seconds()
		}
		InstanceConditionCurrentStatusSeconds.WithLabelValues(
			gvrKey, ns, name,
			string(cond.Type), string(cond.Status), reason,
		).Set(durationSeconds)

		oldCond, existed := initialByType[cond.Type]
		if !existed || oldCond.Status != cond.Status {
			oldStatus := "none"
			if existed {
				oldStatus = string(oldCond.Status)
			}
			log.V(2).Info("condition transitioned",
				"gvr", gvrKey,
				"namespace", ns,
				"name", name,
				"condition_type", string(cond.Type),
				"old_status", oldStatus,
				"new_status", string(cond.Status),
				"reason", reason,
			)
		}
	}

	// Clean up gauges for conditions that disappeared.
	for _, oldCond := range initialConditions {
		if _, stillPresent := finalTypes[oldCond.Type]; !stillPresent {
			InstanceConditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
				labelGVR:           gvrKey,
				labelNamespace:     ns,
				labelName:          name,
				labelConditionType: string(oldCond.Type),
			})
		}
	}
}

// deleteInstanceMetrics removes all gauge metrics for a specific instance.
// Called when an instance is deleted (not found).
func DeleteInstanceMetrics(gvr schema.GroupVersionResource, namespace, name string) {
	InstanceConditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
		labelGVR:       gvr.String(),
		labelNamespace: namespace,
		labelName:      name,
	})
}

// DeleteGVRMetrics removes all gauge metrics for all instances of a GVR.
// Called when an RGD is deregistered (deleted).
func DeleteGVRMetrics(gvr schema.GroupVersionResource) {
	InstanceConditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
		labelGVR: gvr.String(),
	})
}

// indexConditionsByType builds a lookup map from condition type to condition.
func indexConditionsByType(conditions []v1alpha1.Condition) map[v1alpha1.ConditionType]v1alpha1.Condition {
	m := make(map[v1alpha1.ConditionType]v1alpha1.Condition, len(conditions))
	for _, c := range conditions {
		m[c.Type] = c
	}
	return m
}
