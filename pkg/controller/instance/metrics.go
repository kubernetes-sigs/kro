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
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const (
	labelGVR             = "gvr"
	labelNamespace       = "namespace"
	labelName            = "name"
	labelConditionType   = "condition_type"
	labelConditionStatus = "condition_status"
	labelReason          = "reason"
)

func init() {
	metrics.Registry.MustRegister(
		conditionCurrentStatusSeconds,
		conditionTransitionsTotal,
	)
}

var (
	conditionCurrentStatusSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kro_instance_condition_current_status_seconds",
			Help: "The current amount of time in seconds that an instance status condition has been in a specific state.",
		},
		[]string{labelGVR, labelNamespace, labelName, labelConditionType, labelConditionStatus, labelReason},
	)

	conditionTransitionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kro_instance_condition_transitions_total",
			Help: "Total number of instance status condition transitions.",
		},
		[]string{labelGVR, labelConditionType, labelConditionStatus, labelReason},
	)
)

// emitConditionTelemetry computes the diff between initial and final conditions,
// emits gauge metrics for all current conditions, emits transition counters for
// changed conditions, fires K8s Events and structured logs for transitions, and
// cleans up gauges for disappeared conditions.
func emitConditionTelemetry(
	log logr.Logger,
	recorder record.EventRecorder,
	gvr schema.GroupVersionResource,
	inst *unstructured.Unstructured,
	initialConditions []v1alpha1.Condition,
	finalConditions []v1alpha1.Condition,
) {
	gvrKey := gvrToKey(gvr)
	ns := inst.GetNamespace()
	name := inst.GetName()

	initialByType := make(map[v1alpha1.ConditionType]v1alpha1.Condition, len(initialConditions))
	for _, c := range initialConditions {
		initialByType[c.Type] = c
	}

	finalTypes := make(map[v1alpha1.ConditionType]struct{}, len(finalConditions))

	for _, cond := range finalConditions {
		finalTypes[cond.Type] = struct{}{}
		reason := derefString(cond.Reason)

		var durationSeconds float64
		if cond.LastTransitionTime != nil {
			durationSeconds = time.Since(cond.LastTransitionTime.Time).Seconds()
		}
		conditionCurrentStatusSeconds.WithLabelValues(
			gvrKey, ns, name,
			string(cond.Type), string(cond.Status), reason,
		).Set(durationSeconds)

		oldCond, existed := initialByType[cond.Type]
		if !existed || oldCond.Status != cond.Status {
			conditionTransitionsTotal.WithLabelValues(
				gvrKey, string(cond.Type), string(cond.Status), reason,
			).Inc()

			oldStatus := "none"
			if existed {
				oldStatus = string(oldCond.Status)
			}
			log.Info("condition transitioned",
				"gvr", gvrKey,
				"namespace", ns,
				"name", name,
				"condition_type", string(cond.Type),
				"old_status", oldStatus,
				"new_status", string(cond.Status),
				"reason", reason,
			)

			if recorder != nil {
				recorder.Eventf(inst, "Normal", string(cond.Type),
					"Status condition transitioned, Type: %s, Status: %s -> %s, Reason: %s",
					cond.Type, oldStatus, cond.Status, reason,
				)
			}
		}
	}

	for _, oldCond := range initialConditions {
		if _, stillPresent := finalTypes[oldCond.Type]; !stillPresent {
			conditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
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
func deleteInstanceMetrics(gvr schema.GroupVersionResource, namespace, name string) {
	conditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
		labelGVR:       gvrToKey(gvr),
		labelNamespace: namespace,
		labelName:      name,
	})
}

// DeleteGVRMetrics removes all gauge metrics for all instances of a GVR.
// Called when an RGD is deregistered (deleted).
func DeleteGVRMetrics(gvr schema.GroupVersionResource) {
	conditionCurrentStatusSeconds.DeletePartialMatch(prometheus.Labels{
		labelGVR: gvrToKey(gvr),
	})
}

// gvrToKey returns a compact string key for the GVR.
func gvrToKey(gvr schema.GroupVersionResource) string {
	var b strings.Builder
	if gvr.Group != "" {
		b.WriteString(gvr.Group)
	}
	if gvr.Version != "" {
		b.WriteByte('/')
		b.WriteString(gvr.Version)
	}
	if gvr.Resource != "" {
		b.WriteByte('/')
		b.WriteString(gvr.Resource)
	}
	return b.String()
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// conditionsFromInstance extracts conditions from an unstructured instance object.
func conditionsFromInstance(inst *unstructured.Unstructured) []v1alpha1.Condition {
	if inst == nil {
		return nil
	}
	return (&unstructuredWrapper{inst}).GetConditions()
}

