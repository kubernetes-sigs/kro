// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Forked from
// https://github.com/knative/pkg/blob/9f3e60a9244cb08be00ba780f3683bbe70eac159/apis/condition_set_impl_test.go
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
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ----------------- Test Resource ----------------------

type TestResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	runtime.Object // hack to adhere to the Object contract.

	c []Condition
}

// hack to adhere to the Object contract.

func (*TestResource) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}
func (tr *TestResource) DeepCopyObject() runtime.Object {
	return tr
}

func (tr *TestResource) GetConditions() []Condition {
	return tr.c
}

func (tr *TestResource) SetConditions(c []Condition) {
	tr.c = c
}

// ------------------------------------------------------

var ignoreFields = cmpopts.IgnoreFields(Condition{}, "LastTransitionTime")

func TestGetCondition(t *testing.T) {
	ready := NewReadyConditions()
	cases := []struct {
		name   string
		dut    Object
		get    string
		expect *Condition
	}{{
		name: "simple",
		dut: &TestResource{c: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}}},
		get: ConditionReady,
		expect: &Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}, {
		name:   "nil",
		dut:    nil,
		get:    ConditionReady,
		expect: nil,
	}, {
		name: "missing",
		dut: &TestResource{c: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}}},
		get:    "Missing",
		expect: nil,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, a := tc.expect, ready.For(tc.dut).Get(tc.get)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
	}
}

func TestSetCondition(t *testing.T) {
	ready := NewReadyConditions()
	cases := []struct {
		name   string
		dut    Object
		set    Condition
		expect *Condition
	}{{
		name: "simple",
		dut: &TestResource{c: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}}},
		set: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: &Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}, {
		name: "nil",
		dut:  nil,
		set: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: nil,
	}, {
		name: "empty",
		dut:  &TestResource{},
		set: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: &Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready.For(tc.dut).Set(tc.set)
			e, a := tc.expect, ready.For(tc.dut).Get(tc.set.Type)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
	}
}

func TestRootIsTrue(t *testing.T) {
	cases := []struct {
		name    string
		dut     Object
		cts     ConditionTypes
		isHappy bool
	}{{
		name: "empty accessor should not be ready",
		dut: &TestResource{
			c: []Condition(nil),
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Different condition type should not be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "False condition accessor should not be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionFalse,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Unknown condition accessor should not be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionUnknown,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Missing condition accessor should not be ready",
		dut: &TestResource{
			c: []Condition{{
				Type: ConditionReady,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "True condition accessor should be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: true,
	}, {
		name: "Multiple conditions with ready accessor should be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}, {
				Type:   ConditionReady,
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: true,
	}, {
		name: "Multiple conditions with ready accessor false should not be ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}, {
				Type:   ConditionReady,
				Status: metav1.ConditionFalse,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Multiple conditions with mixed ready accessor, some don't matter, ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}, {
				Type:   "Bar",
				Status: metav1.ConditionFalse,
			}, {
				Type:   ConditionReady,
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: true,
	}, {
		name: "Multiple conditions with mixed ready accessor, some don't matter, not ready",
		dut: &TestResource{
			c: []Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}, {
				Type:   "Bar",
				Status: metav1.ConditionTrue,
			}, {
				Type:   ConditionReady,
				Status: metav1.ConditionFalse,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if e, a := tc.isHappy, tc.cts.For(tc.dut).Root().IsTrue(); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			}
		})
	}
}

func TestUpdateLastTransitionTime(t *testing.T) {
	condSet := NewReadyConditions()

	cases := []struct {
		name       string
		conditions []Condition
		condition  Condition
		update     bool
	}{{
		name: "LastTransitionTime should be set",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}},

		condition: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		update: true,
	}, {
		name: "LastTransitionTime should update",
		conditions: []Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(time.Unix(1337, 0)),
		}},
		condition: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		update: true,
	}, {
		name: "if LastTransitionTime is the only chance, don't do it",
		conditions: []Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(time.Unix(1337, 0)),
		}},

		condition: Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		},
		update: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conds := &TestResource{c: tc.conditions}

			was := condSet.For(conds).Get(tc.condition.Type)
			condSet.For(conds).Set(tc.condition)
			now := condSet.For(conds).Get(tc.condition.Type)

			if e, a := tc.condition.Status, now.Status; e != a {
				t.Errorf("%q expected: %v to match %v", tc.name, e, a)
			}

			if tc.update {
				if e, a := was.LastTransitionTime, now.LastTransitionTime; e == a {
					t.Errorf("%q expected: %v to not match %v", tc.name, e, a)
				}
			} else {
				if e, a := was.LastTransitionTime, now.LastTransitionTime; e != a {
					t.Errorf("%q expected: %v to match %v", tc.name, e, a)
				}
			}
		})
	}
}

func TestResourceConditions(t *testing.T) {
	condSet := NewReadyConditions()

	dut := &TestResource{}

	foo := Condition{
		Type:   "Foo",
		Status: "True",
	}
	bar := Condition{
		Type:   "Bar",
		Status: "True",
	}

	// Add a new condition.
	condSet.For(dut).Set(foo)

	if got, want := len(dut.c), 2; got != want {
		t.Fatalf("Unexpected Condition length; got %d, want %d", got, want)
	}

	// Add a second condition.
	condSet.For(dut).Set(bar)

	if got, want := len(dut.c), 3; got != want {
		t.Fatalf("Unexpected Condition length; got %d, want %d", got, want)
	}
}

// getTypes is a small helped to strip out the used ConditionTypes from []Condition
func getTypes(conds []Condition) []string {
	types := make([]string, 0, len(conds))
	for _, c := range conds {
		types = append(types, c.Type)
	}
	return types
}

type ConditionSetTrueTest struct {
	name           string
	conditions     []Condition
	conditionTypes []string
	set            string
	happy          bool
	happyWant      *Condition
}

func doTestSetTrueAccessor(t *testing.T, cases []ConditionSetTrueTest) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conditionTypes := tc.conditionTypes
			if conditionTypes == nil {
				conditionTypes = getTypes(tc.conditions)
			}
			condSet := NewReadyConditions(conditionTypes...)
			dut := &TestResource{c: tc.conditions}

			condSet.For(dut).SetTrue(tc.set)

			if e, a := tc.happy, condSet.For(dut).Root().IsTrue(); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			} else if !e && tc.happyWant != nil {
				e, a := tc.happyWant, condSet.For(dut).Root()
				if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
					t.Errorf("%s (-want, +got) = %v", tc.name, diff)
				}
			}

			if tc.set == condSet.root {
				// Skip validation the root condition because we can't be sure
				// setting it true was correct. Use tc.happyWant to test that case.
				return
			}

			expected := &Condition{
				Type:   tc.set,
				Status: metav1.ConditionTrue,
				Reason: tc.set,
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
		// Run same test with SetTrueWithReason
		t.Run(tc.name+" with reason", func(t *testing.T) {
			conditionTypes := tc.conditionTypes
			if conditionTypes == nil {
				conditionTypes = getTypes(tc.conditions)
			}
			cts := NewReadyConditions(conditionTypes...)
			dut := &TestResource{c: tc.conditions}

			cts.For(dut).SetTrueWithReason(tc.set, "UnitTest", "calm down, just testing")

			if e, a := tc.happy, cts.For(dut).Root().IsTrue(); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			} else if !e && tc.happyWant != nil {
				e, a := tc.happyWant, cts.For(dut).Root()
				if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
					t.Errorf("%s (-want, +got) = %v", tc.name, diff)
				}
			}

			if tc.set == cts.root {
				// Skip validation the happy condition because we can't be sure
				// seting it true was correct. Use tc.happyWant to test that case.
				return
			}

			expected := &Condition{
				Type:    tc.set,
				Status:  metav1.ConditionTrue,
				Reason:  "UnitTest",
				Message: "calm down, just testing",
			}

			e, a := expected, cts.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
	}
}

func TestSetTrue(t *testing.T) {
	cases := []ConditionSetTrueTest{{
		name:  "no deps",
		set:   ConditionReady,
		happy: true,
	}, {
		name: "existing conditions, turns happy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}},
		set:   ConditionReady,
		happy: true,
	}, {
		name: "with deps, happy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionUnknown,
		}},
		set:   "Foo",
		happy: true,
		happyWant: &Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: "Foo",
		},
	}, {
		name: "with deps, not happy",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ReadyReason",
			Message: "ReadyMsg",
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionTrue,
			Reason:  "BarReason",
			Message: "BarMsg",
		}},
		set:   "Bar",
		happy: false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		},
	}, {
		name: "update dep, turns happy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}},
		set:   "Foo",
		happy: true,
	}, {
		name: "update dep, happy was unknown, turns happy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionUnknown,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}},
		set:   "Foo",
		happy: true,
	}, {
		name: "update dep 1/2, still not happy",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionFalse,
			Reason:  "BarReason",
			Message: "BarMsg",
		}},
		set:   "Foo",
		happy: false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "BarReason",
			Message: "BarMsg",
		},
	}, {
		name: "update dep 1/3, mixed status, still not happy",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionUnknown,
			Reason:  "BarReason",
			Message: "BarMsg",
		}, {
			Type:    "Baz",
			Status:  metav1.ConditionFalse,
			Reason:  "BazReason",
			Message: "BazMsg",
		}},
		set:   "Foo",
		happy: false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "BazReason",
			Message: "BazMsg",
		},
	}, {
		name: "update dep 1/3, unknown status, still not happy",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionUnknown,
			Reason:  "BarReason",
			Message: "BarMsg",
		}, {
			Type:    "Baz",
			Status:  metav1.ConditionUnknown,
			Reason:  "BazReason",
			Message: "BazMsg",
		}},
		set:   "Foo",
		happy: false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "BarReason",
			Message: "BarMsg",
		},
	}, {
		name: "update dep 1/3, unknown status because nil",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  "FooReason",
			Message: "FooMsg",
		}},
		set:            "Foo",
		conditionTypes: []string{"Foo", "Bar", "Baz"},
		happy:          false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "AwaitingReconciliation",
			Message: "condition \"Bar\" is awaiting reconciliation",
		},
	}, {
		name: "all happy but not cover all dependents",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "LongStory",
			Message: "Set manually",
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:            "Foo",
		conditionTypes: []string{"Foo", "Bar"}, // dependents is more than conditions.
		happy:          false,
		happyWant: &Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  "AwaitingReconciliation",
			Message: "condition \"Bar\" is awaiting reconciliation",
		},
	}, {
		name: "all happy and cover all dependents",
		conditions: []Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "LongStory",
			Message: "Set manually",
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}, {
			Type:   "NewCondition",
			Status: metav1.ConditionTrue,
		}},
		set:            "Foo",
		conditionTypes: []string{"Foo"}, // dependents is less than conditions.
		happy:          true,
		happyWant: &Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}}
	doTestSetTrueAccessor(t, cases)
}

type ConditionSetFalseTest struct {
	name       string
	conditions []Condition
	set        string
	unhappy    bool
}

func doTestSetFalseAccessor(t *testing.T, cases []ConditionSetFalseTest) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			condSet := NewReadyConditions(getTypes(tc.conditions)...)
			dut := &TestResource{c: tc.conditions}

			condSet.For(dut).SetFalse(tc.set, "UnitTest", "calm down, just testing")

			if e, a := !tc.unhappy, condSet.For(dut).Root().IsTrue(); e != a {
				t.Errorf("%q expected: %v got: %v", tc.name, e, a)
			}

			expected := &Condition{
				Type:    tc.set,
				Status:  metav1.ConditionFalse,
				Reason:  "UnitTest",
				Message: "calm down, just testing",
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
	}
}

func TestSetFalse(t *testing.T) {
	cases := []ConditionSetFalseTest{{
		name:    "no deps",
		set:     ConditionReady,
		unhappy: true,
	}, {
		name: "existing conditions, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
	}, {
		name: "with deps, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
	}, {
		name: "with deps, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}},
		set:     ConditionReady,
		unhappy: true,
	}, {
		name: "update dep, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:     "Foo",
		unhappy: true,
	}, {
		name: "update dep, happy was unknown, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionUnknown,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}},
		set:     "Foo",
		unhappy: true,
	}, {
		name: "update dep 1/2, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Bar",
			Status: metav1.ConditionTrue,
		}},
		set:     "Foo",
		unhappy: true,
	}}
	doTestSetFalseAccessor(t, cases)
}

type ConditionSetUnknownTest struct {
	name       string
	conditions []Condition
	set        string
	unhappy    bool
	happyIs    metav1.ConditionStatus
}

func doTestSetUnknownAccessor(t *testing.T, cases []ConditionSetUnknownTest) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			condSet := NewReadyConditions(getTypes(tc.conditions)...)
			dut := &TestResource{c: tc.conditions}

			condSet.For(dut).SetUnknownWithReason(tc.set, "UnitTest", "idk, just testing")

			if e, a := !tc.unhappy, condSet.For(dut).Root().IsTrue(); e != a {
				t.Errorf("%q expected IsTrue: %v got: %v", tc.name, e, a)
			}

			if e, a := tc.happyIs, condSet.For(dut).Get(ConditionReady).Status; e != a {
				t.Errorf("%q expected ConditionReady: %v got: %v", tc.name, e, a)
			}

			expected := &Condition{
				Type:    tc.set,
				Status:  metav1.ConditionUnknown,
				Reason:  "UnitTest",
				Message: "idk, just testing",
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields); diff != "" {
				t.Errorf("%s (-want, +got) = %v", tc.name, diff)
			}
		})
	}
}

func TestSetUnknown(t *testing.T) {
	cases := []ConditionSetUnknownTest{{
		name:    "no deps",
		set:     ConditionReady,
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "existing conditions, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "with deps, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "with deps that are false, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}, {
			Type:   "Bar",
			Status: metav1.ConditionFalse,
		}},
		set:     "Foo",
		unhappy: true,
		happyIs: metav1.ConditionFalse,
	}, {
		name: "update dep, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:     "Foo",
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "update dep, happy was unknown, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionUnknown,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionFalse,
		}},
		set:     "Foo",
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "update dep 1/2, turns unhappy",
		conditions: []Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}, {
			Type:   "Bar",
			Status: metav1.ConditionTrue,
		}},
		set:     "Foo",
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}}
	doTestSetUnknownAccessor(t, cases)
}

func TestRemoveNonDependentConditions(t *testing.T) {
	set := NewReadyConditions("Foo")
	dut := &TestResource{}

	condSet := set.For(dut)
	condSet.SetTrue("Foo")
	condSet.SetTrue("Bar")

	if got, want := len(dut.c), 3; got != want {
		t.Errorf("Marking true() = %v, wanted %v", got, want)
	}

	if !condSet.Root().IsTrue() {
		t.Error("IsTrue() = false, wanted true")
	}

	err := condSet.Clear("Bar")
	if err != nil {
		t.Error("Clear condition should not return err", err)
	}
	if got, want := len(dut.c), 2; got != want {
		t.Errorf("Marking true() = %v, wanted %v", got, want)
	}

	if !condSet.Root().IsTrue() {
		t.Error("IsTrue() = false, wanted true")
	}
}

func TestClearConditionWithNilObject(t *testing.T) {
	set := NewReadyConditions("Foo")
	condSet := set.For(nil)

	err := condSet.Clear("Bar")
	if err != nil {
		t.Error("ClearCondition() expected to return nil if status is nil, got", err)
	}
}
