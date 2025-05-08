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

// Forked from:
// https://raw.githubusercontent.com/knative/pkg/97c7258e3a98b81459936bc7a29dc6a9540fa357/apis/condition_set_test.go
/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apis

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNewReadyConditions(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		count int // count includes the root condition type.
	}{{
		name:  "empty",
		types: []string(nil),
		count: 1,
	}, {
		name:  "one",
		types: []string{"Foo"},
		count: 2,
	}, {
		name:  "duplicate in root",
		types: []string{ConditionReady},
		count: 1,
	}, {
		name:  "duplicate in dependents",
		types: []string{"Foo", "Bar", "Foo"},
		count: 3,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := NewReadyConditions(tc.types...)
			if e, a := tc.count, 1+len(set.dependents); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			}
		})
	}
}

func TestNewSucceededConditions(t *testing.T) {
	cases := []struct {
		name  string
		types []string
		count int // count includes the root condition type.
	}{{
		name:  "empty",
		types: []string(nil),
		count: 1,
	}, {
		name:  "one",
		types: []string{"Foo"},
		count: 2,
	}, {
		name:  "duplicate in root",
		types: []string{ConditionSucceeded},
		count: 1,
	}, {
		name:  "duplicate in dependents",
		types: []string{"Foo", "Bar", "Foo"},
		count: 3,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := NewSucceededConditions(tc.types...)
			if e, a := tc.count, 1+len(set.dependents); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			}
		})
	}
}

func TestNonTerminalCondition(t *testing.T) {
	set := NewReadyConditions("Foo")
	dut := &TestResource{}

	condSet := set.For(dut)

	// Setting the other "terminal" condition makes Ready true.
	condSet.SetTrue("Foo")
	if got, want := condSet.Get("Ready").Status, metav1.ConditionTrue; got != want {
		t.Errorf("SetTrue(Foo) = %v, wanted %v", got, want)
	}

	// Setting a "non-terminal" condition, doesn't change Ready.
	condSet.SetUnknown("Bar")
	if got, want := condSet.Get("Ready").Status, metav1.ConditionTrue; got != want {
		t.Errorf("SetUnknown(Bar) = %v, wanted %v", got, want)
	}

	// Setting the other "terminal" condition by Unknown makes Ready false
	condSet.SetUnknown("Foo")
	if got, want := condSet.Get("Ready").Status, metav1.ConditionUnknown; got != want {
		t.Errorf("SetUnknown(Foo) = %v, wanted %v", got, want)
	}

	// Setting the other "terminal" condition by True makes Ready true
	condSet.SetTrueWithReason("Foo", "Reason", "")
	if got, want := condSet.Get("Ready").Status, metav1.ConditionTrue; got != want {
		t.Errorf("SetUnknown(Foo) = %v, wanted %v", got, want)
	}

	// Setting a "non-terminal" condition, doesn't change Ready.
	condSet.SetFalse("Bar", "", "")
	if got, want := condSet.Get("Ready").Status, metav1.ConditionTrue; got != want {
		t.Errorf("SetFalse(Bar) = %v, wanted %v", got, want)
	}
}
