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

package apis

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestIsRootReady(t *testing.T) {
	cases := []struct {
		name       string
		dut        Object
		generation int64
		conditions []v1alpha1.Condition
		want       bool
	}{{
		name: "nil object",
		dut:  nil,
		want: false,
	}, {
		name:       "root true and observed generation matches",
		generation: 3,
		conditions: []v1alpha1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 3,
		}},
		want: true,
	}, {
		name:       "root true but stale observed generation",
		generation: 4,
		conditions: []v1alpha1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: 3,
		}},
		want: false,
	}, {
		name:       "root false",
		generation: 1,
		conditions: []v1alpha1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: 1,
		}},
		want: false,
	}}

	set := NewReadyConditions()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dut Object
			if tc.name != "nil object" {
				r := &TestResource{c: tc.conditions}
				r.Generation = tc.generation
				dut = r
			}
			if got := set.For(dut).IsRootReady(); got != tc.want {
				t.Errorf("IsRootReady() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestList(t *testing.T) {
	cases := []struct {
		name string
		nil  bool
		want int
	}{{
		name: "nil object returns nil",
		nil:  true,
		want: 0,
	}, {
		name: "non-nil object lists conditions",
		want: 1, // Ready initialized by For.
	}}

	set := NewReadyConditions()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dut Object
			if !tc.nil {
				dut = &TestResource{}
			}
			if got := len(set.For(dut).List()); got != tc.want {
				t.Errorf("len(List()) = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAsUnstructured(t *testing.T) {
	set := NewReadyConditions("Foo")
	dut := &TestResource{}
	cs := set.For(dut)
	cs.SetTrue("Foo")

	raw := cs.AsUnstructured()
	if len(raw) != len(dut.GetConditions()) {
		t.Fatalf("AsUnstructured() length = %d, want %d", len(raw), len(dut.GetConditions()))
	}
	for i, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("item %d is not a map: %T", i, item)
		}
		if _, ok := m["type"]; !ok {
			t.Errorf("item %d missing 'type' key: %v", i, m)
		}
	}

	// nil object yields empty slice.
	if got := set.For(nil).AsUnstructured(); len(got) != 0 {
		t.Errorf("AsUnstructured() on nil = %v, want empty", got)
	}
}

func TestConditionSetIsTrue(t *testing.T) {
	cases := []struct {
		name       string
		conditions []v1alpha1.Condition
		query      []string
		want       bool
	}{{
		name: "all true",
		conditions: []v1alpha1.Condition{
			{Type: "Foo", Status: metav1.ConditionTrue},
			{Type: "Bar", Status: metav1.ConditionTrue},
		},
		query: []string{"Foo", "Bar"},
		want:  true,
	}, {
		name: "one false",
		conditions: []v1alpha1.Condition{
			{Type: "Foo", Status: metav1.ConditionTrue},
			{Type: "Bar", Status: metav1.ConditionFalse},
		},
		query: []string{"Foo", "Bar"},
		want:  false,
	}, {
		name: "missing condition",
		conditions: []v1alpha1.Condition{
			{Type: "Foo", Status: metav1.ConditionTrue},
		},
		query: []string{"Missing"},
		want:  false,
	}, {
		name:  "no query types",
		query: nil,
		want:  true,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := NewReadyConditions()
			dut := &TestResource{c: tc.conditions}
			if got := set.For(dut).IsTrue(tc.query...); got != tc.want {
				t.Errorf("IsTrue(%v) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestRootNilObject(t *testing.T) {
	set := NewReadyConditions()
	if got := set.For(nil).Root(); got != nil {
		t.Errorf("Root() on nil object = %v, want nil", got)
	}
}

func TestSetNilObject(t *testing.T) {
	set := NewReadyConditions()
	if got := set.For(nil).Set(v1alpha1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue}); got {
		t.Errorf("Set() on nil object = %v, want false", got)
	}
}

func TestClearAbsentCondition(t *testing.T) {
	set := NewReadyConditions("Foo")
	dut := &TestResource{}
	cs := set.For(dut)

	// Clearing a non-existent independent condition is a no-op (nil error).
	if err := cs.Clear("DoesNotExist"); err != nil {
		t.Errorf("Clear(absent) = %v, want nil", err)
	}
}

func TestPruneConditionsNoMatch(t *testing.T) {
	// Prunes targets a type that is not present: the filtered slice equals the
	// original length, so SetConditions must not be called with a changed set.
	set := NewReadyConditions("Foo").Prunes("Nonexistent")
	dut := &TestResource{}
	dut.SetConditions([]v1alpha1.Condition{
		{Type: "Foo", Status: metav1.ConditionTrue},
	})

	set.For(dut)

	found := false
	for _, c := range dut.GetConditions() {
		if string(c.Type) == "Foo" {
			found = true
		}
	}
	if !found {
		t.Error("Foo condition should be preserved when prune target is absent")
	}
}

// TestFindMostUnhealthyNilTransitionTimes exercises the nil LastTransitionTime
// branches in findUnhealthyDependents/findMostUnhealthy sort comparators by
// mixing conditions with and without transition times.
func TestFindMostUnhealthyNilTransitionTimes(t *testing.T) {
	cases := []struct {
		name       string
		conditions []v1alpha1.Condition
		wantStatus metav1.ConditionStatus
	}{{
		name: "false with nil time wins over unknown",
		conditions: []v1alpha1.Condition{
			{Type: ConditionReady, Status: metav1.ConditionFalse},
			{Type: "Foo", Status: metav1.ConditionFalse},
			{Type: "Bar", Status: metav1.ConditionUnknown,
				LastTransitionTime: new(metav1.NewTime(time.Unix(1000, 0)))},
		},
		wantStatus: metav1.ConditionFalse,
	}, {
		name: "mixed nil and non-nil times, both unknown",
		conditions: []v1alpha1.Condition{
			{Type: "Foo", Status: metav1.ConditionUnknown},
			{Type: "Bar", Status: metav1.ConditionUnknown,
				LastTransitionTime: new(metav1.NewTime(time.Unix(2000, 0)))},
		},
		wantStatus: metav1.ConditionUnknown,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := NewReadyConditions("Foo", "Bar")
			dut := &TestResource{c: tc.conditions}
			cs := set.For(dut)
			// Trigger recompute by re-setting the Foo dependent.
			cs.Set(v1alpha1.Condition{Type: "Foo", Status: cs.Get("Foo").Status})
			if got := cs.Root().Status; got != tc.wantStatus {
				t.Errorf("root status = %v, want %v", got, tc.wantStatus)
			}
		})
	}
}
