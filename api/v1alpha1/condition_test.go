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

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestCondition_GetStatus(t *testing.T) {
	tests := []struct {
		name string
		c    *Condition
		want metav1.ConditionStatus
	}{{
		name: "Status True",
		c:    &Condition{Status: metav1.ConditionTrue},
		want: metav1.ConditionTrue,
	}, {
		name: "Status False",
		c:    &Condition{Status: metav1.ConditionFalse},
		want: metav1.ConditionFalse,
	}, {
		name: "Status Unknown",
		c:    &Condition{Status: metav1.ConditionUnknown},
		want: metav1.ConditionUnknown,
	}, {
		name: "Status Nil",
		want: metav1.ConditionUnknown,
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.GetStatus(); got != tt.want {
				t.Errorf("GetStatus() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCondition_IsFalse(t *testing.T) {
	tests := []struct {
		name string
		c    *Condition
		want bool
	}{{
		name: "Status True",
		c:    &Condition{Status: metav1.ConditionTrue},
		want: false,
	}, {
		name: "Status False",
		c:    &Condition{Status: metav1.ConditionFalse},
		want: true,
	}, {
		name: "Status Unknown",
		c:    &Condition{Status: metav1.ConditionUnknown},
		want: false,
	}, {
		name: "Status Nil",
		want: false,
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.IsFalse(); got != tt.want {
				t.Errorf("IsFalse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCondition_IsTrue(t *testing.T) {
	tests := []struct {
		name string
		c    *Condition
		want bool
	}{{
		name: "Status True",
		c:    &Condition{Status: metav1.ConditionTrue},
		want: true,
	}, {
		name: "Status False",
		c:    &Condition{Status: metav1.ConditionFalse},
		want: false,
	}, {
		name: "Status Unknown",
		c:    &Condition{Status: metav1.ConditionUnknown},
		want: false,
	}, {
		name: "Status Nil",
		want: false,
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.IsTrue(); got != tt.want {
				t.Errorf("IsTrue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCondition_IsUnknown(t *testing.T) {
	tests := []struct {
		name string
		c    *Condition
		want bool
	}{{
		name: "Status True",
		c:    &Condition{Status: metav1.ConditionTrue},
		want: false,
	}, {
		name: "Status False",
		c:    &Condition{Status: metav1.ConditionFalse},
		want: false,
	}, {
		name: "Status Unknown",
		c:    &Condition{Status: metav1.ConditionUnknown},
		want: true,
	}, {
		name: "Status Nil",
		want: true,
	},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.IsUnknown(); got != tt.want {
				t.Errorf("IsUnknown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConditionRoundTrip(t *testing.T) {
	now := metav1.Now()
	originalCondition := Condition{
		Type:               ConditionType("TestCondition"),
		Status:             metav1.ConditionTrue,
		Reason:             ptr.To("TestReason"),
		Message:            ptr.To("Test message"),
		LastTransitionTime: &now,
		ObservedGeneration: 42,
	}

	t.Logf("Original condition: Type=%s, Status=%s, LastTransitionTime=%v, ObservedGeneration=%d",
		originalCondition.Type, originalCondition.Status, originalCondition.LastTransitionTime, originalCondition.ObservedGeneration)

	// Test the round trip that happens in our unstructured condition adapter:
	// 1. []v1alpha1.Condition -> JSON -> []interface{} (SetConditions)
	// 2. []interface{} -> JSON -> []v1alpha1.Condition (GetConditions)

	conditions := []Condition{originalCondition}

	// Step 1: Marshal conditions to JSON (what SetConditions does internally)
	conditionsJSON, err := json.Marshal(conditions)
	if err != nil {
		t.Fatalf("Failed to marshal conditions: %v", err)
	}
	t.Logf("Marshaled JSON: %s", string(conditionsJSON))

	// Step 2: Unmarshal to []interface{} (what SetConditions stores in unstructured)
	var conditionsInterface []interface{}
	if err := json.Unmarshal(conditionsJSON, &conditionsInterface); err != nil {
		t.Fatalf("Failed to unmarshal to interface{} slice: %v", err)
	}
	t.Logf("Interface slice: %+v", conditionsInterface)

	// Step 3: Marshal the interface slice back to JSON (what GetConditions starts with)
	interfaceJSON, err := json.Marshal(conditionsInterface)
	if err != nil {
		t.Fatalf("Failed to marshal interface{} slice: %v", err)
	}
	t.Logf("Re-marshaled JSON: %s", string(interfaceJSON))

	// Step 4: Unmarshal back to []v1alpha1.Condition (what GetConditions returns)
	var finalConditions []Condition
	if err := json.Unmarshal(interfaceJSON, &finalConditions); err != nil {
		t.Fatalf("Failed to unmarshal to final conditions: %v", err)
	}

	if len(finalConditions) != 1 {
		t.Fatalf("Expected 1 condition, got %d", len(finalConditions))
	}

	finalCondition := finalConditions[0]
	t.Logf("Final condition: Type=%s, Status=%s, LastTransitionTime=%v, ObservedGeneration=%d",
		finalCondition.Type, finalCondition.Status, finalCondition.LastTransitionTime, finalCondition.ObservedGeneration)

	// Validate all fields survived the round trip
	if finalCondition.Type != originalCondition.Type {
		t.Errorf("Type lost in round trip: expected %s, got %s", originalCondition.Type, finalCondition.Type)
	}

	if finalCondition.Status != originalCondition.Status {
		t.Errorf("Status lost in round trip: expected %s, got %s", originalCondition.Status, finalCondition.Status)
	}

	if finalCondition.ObservedGeneration != originalCondition.ObservedGeneration {
		t.Errorf("ObservedGeneration lost in round trip: expected %d, got %d",
			originalCondition.ObservedGeneration, finalCondition.ObservedGeneration)
	}

	if originalCondition.Reason != nil {
		if finalCondition.Reason == nil {
			t.Error("Reason was lost in round trip")
		} else if *finalCondition.Reason != *originalCondition.Reason {
			t.Errorf("Reason changed in round trip: expected %s, got %s",
				*originalCondition.Reason, *finalCondition.Reason)
		}
	}

	if originalCondition.Message != nil {
		if finalCondition.Message == nil {
			t.Error("Message was lost in round trip")
		} else if *finalCondition.Message != *originalCondition.Message {
			t.Errorf("Message changed in round trip: expected %s, got %s",
				*originalCondition.Message, *finalCondition.Message)
		}
	}

	// The critical test: LastTransitionTime preservation
	if originalCondition.LastTransitionTime != nil {
		if finalCondition.LastTransitionTime == nil {
			t.Fatal("LastTransitionTime was lost during round trip")
		}

		originalTime := originalCondition.LastTransitionTime.Time
		finalTime := finalCondition.LastTransitionTime.Time

		// Times should be equal within a reasonable tolerance (JSON loses nanosecond precision)
		if originalTime.Truncate(time.Second) != finalTime.Truncate(time.Second) {
			t.Errorf("LastTransitionTime changed significantly during round trip: expected %v, got %v",
				originalTime, finalTime)
		}

		// Verify the time is properly formatted in the JSON
		if !finalTime.After(time.Time{}) {
			t.Error("Final LastTransitionTime is zero/invalid")
		}
	}
}

func TestConditionRoundTripWithoutOptionalFields(t *testing.T) {
	// Test a minimal condition with only required fields
	originalCondition := Condition{
		Type:               ConditionType("MinimalCondition"),
		Status:             metav1.ConditionFalse,
		ObservedGeneration: 1,
		// No Reason, Message, or LastTransitionTime
	}

	conditions := []Condition{originalCondition}

	// Perform the same round trip
	conditionsJSON, _ := json.Marshal(conditions)
	var conditionsInterface []interface{}
	_ = json.Unmarshal(conditionsJSON, &conditionsInterface)
	interfaceJSON, _ := json.Marshal(conditionsInterface)
	var finalConditions []Condition
	_ = json.Unmarshal(interfaceJSON, &finalConditions)

	if len(finalConditions) != 1 {
		t.Fatalf("Expected 1 condition, got %d", len(finalConditions))
	}

	finalCondition := finalConditions[0]

	// Validate required fields
	if finalCondition.Type != originalCondition.Type {
		t.Errorf("Type lost: expected %s, got %s", originalCondition.Type, finalCondition.Type)
	}
	if finalCondition.Status != originalCondition.Status {
		t.Errorf("Status lost: expected %s, got %s", originalCondition.Status, finalCondition.Status)
	}
	if finalCondition.ObservedGeneration != originalCondition.ObservedGeneration {
		t.Errorf("ObservedGeneration lost: expected %d, got %d",
			originalCondition.ObservedGeneration, finalCondition.ObservedGeneration)
	}

	// Optional fields should remain nil
	if finalCondition.Reason != nil {
		t.Errorf("Reason should be nil but got %v", *finalCondition.Reason)
	}
	if finalCondition.Message != nil {
		t.Errorf("Message should be nil but got %v", *finalCondition.Message)
	}
	if finalCondition.LastTransitionTime != nil {
		t.Errorf("LastTransitionTime should be nil but got %v", finalCondition.LastTransitionTime)
	}
}
