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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/record"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func strPtr(s string) *string { return &s }

func newTestInst() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "WebApp",
			"metadata": map[string]interface{}{
				"name":      "test-inst",
				"namespace": "default",
			},
		},
	}
}

func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestEmitConditionEvents_NoChange(t *testing.T) {
	recorder := record.NewFakeRecorder(100)
	inst := newTestInst()

	conditions := []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: strPtr("AllGood")},
	}

	emitConditionEvents(recorder, inst, conditions, conditions)
	assert.Empty(t, drainEvents(recorder), "no events expected when conditions are unchanged")
}

func TestEmitConditionEvents_NewCondition(t *testing.T) {
	tests := []struct {
		name     string
		status   metav1.ConditionStatus
		wantType string
		wantOld  string
	}{
		{"True", metav1.ConditionTrue, corev1.EventTypeNormal, "none -> True"},
		{"False", metav1.ConditionFalse, corev1.EventTypeWarning, "none -> False"},
		{"Unknown", metav1.ConditionUnknown, corev1.EventTypeWarning, "none -> Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(100)
			inst := newTestInst()

			final := []v1alpha1.Condition{
				{Type: "Ready", Status: tt.status, Reason: strPtr("TestReason")},
			}

			emitConditionEvents(recorder, inst, nil, final)

			events := drainEvents(recorder)
			require.Len(t, events, 1)
			assert.Contains(t, events[0], tt.wantType)
			assert.Contains(t, events[0], tt.wantOld)
		})
	}
}

func TestEmitConditionEvents_TrueToFalseIsWarning(t *testing.T) {
	recorder := record.NewFakeRecorder(100)
	inst := newTestInst()

	initial := []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: strPtr("AllGood")},
	}
	final := []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: strPtr("Degraded")},
	}

	emitConditionEvents(recorder, inst, initial, final)

	events := drainEvents(recorder)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], corev1.EventTypeWarning)
	assert.Contains(t, events[0], "True -> False")
	assert.Contains(t, events[0], "Degraded")
}

func TestEmitConditionEvents_FalseToTrueIsNormal(t *testing.T) {
	recorder := record.NewFakeRecorder(100)
	inst := newTestInst()

	initial := []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionFalse, Reason: strPtr("Degraded")},
	}
	final := []v1alpha1.Condition{
		{Type: "Ready", Status: metav1.ConditionTrue, Reason: strPtr("Recovered")},
	}

	emitConditionEvents(recorder, inst, initial, final)

	events := drainEvents(recorder)
	require.Len(t, events, 1)
	assert.Contains(t, events[0], corev1.EventTypeNormal)
	assert.Contains(t, events[0], "False -> True")
	assert.Contains(t, events[0], "Recovered")
}

func TestEmitConditionEvents_MultipleConditions(t *testing.T) {
	recorder := record.NewFakeRecorder(100)
	inst := newTestInst()

	initial := []v1alpha1.Condition{
		{Type: "InstanceManaged", Status: metav1.ConditionTrue, Reason: strPtr("OK")},
		{Type: "ResourcesReady", Status: metav1.ConditionFalse, Reason: strPtr("Pending")},
	}
	final := []v1alpha1.Condition{
		{Type: "InstanceManaged", Status: metav1.ConditionTrue, Reason: strPtr("OK")},      // no change
		{Type: "ResourcesReady", Status: metav1.ConditionTrue, Reason: strPtr("AllReady")}, // changed
		{Type: "GraphResolved", Status: metav1.ConditionTrue, Reason: strPtr("Resolved")},  // new
	}

	emitConditionEvents(recorder, inst, initial, final)

	events := drainEvents(recorder)
	require.Len(t, events, 2, "should only emit events for changed/new conditions")

	eventTypes := make(map[string]bool)
	for _, e := range events {
		if strings.Contains(e, "ResourcesReady") {
			eventTypes["ResourcesReady"] = true
		}
		if strings.Contains(e, "GraphResolved") {
			eventTypes["GraphResolved"] = true
		}
	}
	assert.True(t, eventTypes["ResourcesReady"])
	assert.True(t, eventTypes["GraphResolved"])
}
