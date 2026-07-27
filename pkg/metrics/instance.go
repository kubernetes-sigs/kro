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

	// InstanceConditionCurrentStatusSeconds is computed at scrape time by a
	// Collector so it stays accurate for non-PromQL consumers (CloudWatch,
	// Datadog) even while the controller is idle. The cache rebuilds from
	// etcd as instances reconcile after a restart.
	InstanceConditionCurrentStatusSeconds = newDurationCollector(
		"instance_condition_current_status_seconds",
		"The current amount of time in seconds that an instance status condition has been in a specific state.",
	)
)

// EmitConditionMetrics syncs the duration cache with an instance's
// conditions. For each final condition it caches the LastTransitionTime
// keyed by condition type, and evicts conditions that no longer appear.
// It also logs status transitions.
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

		var lastTransition time.Time
		if cond.LastTransitionTime != nil {
			lastTransition = cond.LastTransitionTime.Time
		}

		// Cache overwrites any previous status/reason for this condition
		// type, so a transition replaces the old series without a separate
		// eviction step.
		InstanceConditionCurrentStatusSeconds.Cache(
			conditionKey{
				gvr:           gvrKey,
				namespace:     ns,
				name:          name,
				conditionType: string(cond.Type),
			},
			string(cond.Status),
			reason,
			lastTransition,
		)

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

	for _, oldCond := range initialConditions {
		if _, stillPresent := finalTypes[oldCond.Type]; !stillPresent {
			InstanceConditionCurrentStatusSeconds.EvictKey(conditionKey{
				gvr:           gvrKey,
				namespace:     ns,
				name:          name,
				conditionType: string(oldCond.Type),
			})
		}
	}
}

// DeleteInstanceMetrics removes all metrics for a specific instance.
// Called when an instance is deleted (not found).
func DeleteInstanceMetrics(gvr schema.GroupVersionResource, namespace, name string) {
	InstanceConditionCurrentStatusSeconds.EvictInstance(gvr.String(), namespace, name)
}

// DeleteGVRMetrics removes all metrics for all instances of a GVR.
// Called when an RGD is deregistered (deleted).
func DeleteGVRMetrics(gvr schema.GroupVersionResource) {
	InstanceConditionCurrentStatusSeconds.EvictGVR(gvr.String())
}

// indexConditionsByType builds a lookup map from condition type to condition.
func indexConditionsByType(conditions []v1alpha1.Condition) map[v1alpha1.ConditionType]v1alpha1.Condition {
	m := make(map[v1alpha1.ConditionType]v1alpha1.Condition, len(conditions))
	for _, c := range conditions {
		m[c.Type] = c
	}
	return m
}
