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
	"k8s.io/utils/ptr"

	"github.com/kro-run/kro/api/v1alpha1"
)

// ----------------- Test Resource ----------------------

type TestResource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	runtime.Object // hack to adhere to the Object contract.

	c []v1alpha1.Condition
}

// hack to adhere to the Object contract.

func (*TestResource) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}
func (tr *TestResource) DeepCopyObject() runtime.Object {
	return tr
}

func (tr *TestResource) GetConditions() []v1alpha1.Condition {
	return tr.c
}

func (tr *TestResource) SetConditions(c []v1alpha1.Condition) {
	tr.c = c
}

// ------------------------------------------------------

// Custom comparer for pointer fields
var conditionComparer = cmp.Comparer(func(x, y v1alpha1.Condition) bool {
	if x.Type != y.Type || x.Status != y.Status || x.ObservedGeneration != y.ObservedGeneration {
		return false
	}

	// Compare Reason pointers
	if !ptr.Equal(x.Reason, y.Reason) {
		return false
	}

	// Compare Message pointers
	if (x.Message == nil) != (y.Message == nil) {
		return false
	}
	if x.Message != nil && y.Message != nil && *x.Message != *y.Message {
		return false
	}

	// Ignore LastTransitionTime
	return true
})

// Include LastTransitionTime in fields to ignore
var ignoreFields = cmpopts.IgnoreFields(v1alpha1.Condition{}, "LastTransitionTime")

func TestGetCondition(t *testing.T) {
	ready := NewReadyConditions()
	cases := []struct {
		name   string
		dut    Object
		get    string
		expect *v1alpha1.Condition
	}{{
		name: "simple",
		dut: &TestResource{c: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionTrue,
			Message: ptr.To(""),
		}}},
		get: ConditionReady,
		expect: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionTrue,
			Message: ptr.To(""),
		},
	}, {
		name:   "nil",
		dut:    nil,
		get:    ConditionReady,
		expect: nil,
	}, {
		name: "missing",
		dut: &TestResource{c: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionTrue,
			Message: ptr.To(""),
		}}},
		get:    "Missing",
		expect: nil,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, a := tc.expect, ready.For(tc.dut).Get(tc.get)
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
		set    v1alpha1.Condition
		expect *v1alpha1.Condition
	}{{
		name: "simple",
		dut: &TestResource{c: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}}},
		set: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: &v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}, {
		name: "nil",
		dut:  nil,
		set: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: nil,
	}, {
		name: "empty",
		dut:  &TestResource{},
		set: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		expect: &v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready.For(tc.dut).Set(tc.set)
			e, a := tc.expect, ready.For(tc.dut).Get(string(tc.set.Type))
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
			c: []v1alpha1.Condition(nil),
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Different condition type should not be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
				Type:   "Foo",
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "False condition accessor should not be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionFalse,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Unknown condition accessor should not be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionUnknown,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "Missing condition accessor should not be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
				Type: ConditionReady,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: false,
	}, {
		name: "True condition accessor should be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
				Type:   ConditionReady,
				Status: metav1.ConditionTrue,
			}},
		},
		cts:     NewReadyConditions(),
		isHappy: true,
	}, {
		name: "Multiple conditions with ready accessor should be ready",
		dut: &TestResource{
			c: []v1alpha1.Condition{{
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
			c: []v1alpha1.Condition{{
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
			c: []v1alpha1.Condition{{
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
			c: []v1alpha1.Condition{{
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
		conditions []v1alpha1.Condition
		condition  v1alpha1.Condition
		update     bool
	}{{
		name: "LastTransitionTime should be set",
		conditions: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}},

		condition: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		update: true,
	}, {
		name: "LastTransitionTime should update",
		conditions: []v1alpha1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: ptr.To(metav1.NewTime(time.Unix(1337, 0))),
		}},
		condition: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
		update: true,
	}, {
		name: "if LastTransitionTime is the only chance, don't do it",
		conditions: []v1alpha1.Condition{{
			Type:               ConditionReady,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: ptr.To(metav1.NewTime(time.Unix(1337, 0))),
		}},

		condition: v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		},
		update: false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conds := &TestResource{c: tc.conditions}

			was := condSet.For(conds).Get(string(tc.condition.Type))
			condSet.For(conds).Set(tc.condition)
			now := condSet.For(conds).Get(string(tc.condition.Type))

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

	foo := v1alpha1.Condition{
		Type:   "Foo",
		Status: "True",
	}
	bar := v1alpha1.Condition{
		Type:   "Bar",
		Status: "True",
	}

	// Add a new condition.
	condSet.For(dut).Set(foo)

	if got, want := len(dut.c), 2; got != want {
		t.Fatalf("Unexpected condition length; got %d, want %d", got, want)
	}

	// Add a second condition.
	condSet.For(dut).Set(bar)

	if got, want := len(dut.c), 3; got != want {
		t.Fatalf("Unexpected condition length; got %d, want %d", got, want)
	}
}

// getTypes is a small helped to strip out the used ConditionTypes from []v1alpha1.Condition
func getTypes(conds []v1alpha1.Condition) []string {
	types := make([]string, 0, len(conds))
	for _, c := range conds {
		types = append(types, string(c.Type))
	}
	return types
}

type ConditionSetTrueTest struct {
	name           string
	conditions     []v1alpha1.Condition
	conditionTypes []string
	set            string
	happy          bool
	happyWant      *v1alpha1.Condition
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
				if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
					t.Errorf("%s (-want, +got) = %v", tc.name, diff)
				}
			}

			if tc.set == condSet.root {
				// Skip validation the root condition because we can't be sure
				// setting it true was correct. Use tc.happyWant to test that case.
				return
			}

			expected := &v1alpha1.Condition{
				Type:    v1alpha1.ConditionType(tc.set),
				Status:  metav1.ConditionTrue,
				Reason:  ptr.To(tc.set),
				Message: ptr.To(""),
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
				if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
					t.Errorf("%s (-want, +got) = %v", tc.name, diff)
				}
			}

			if tc.set == cts.root {
				// Skip validation the happy condition because we can't be sure
				// seting it true was correct. Use tc.happyWant to test that case.
				return
			}

			expected := &v1alpha1.Condition{
				Type:    v1alpha1.ConditionType(tc.set),
				Status:  metav1.ConditionTrue,
				Reason:  ptr.To("UnitTest"),
				Message: ptr.To("calm down, just testing"),
			}

			e, a := expected, cts.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
		conditions: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}},
		set:   ConditionReady,
		happy: true,
	}, {
		name: "with deps, happy",
		conditions: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionFalse,
		}, {
			Type:   "Foo",
			Status: metav1.ConditionUnknown,
		}},
		set:   "Foo",
		happy: true,
		happyWant: &v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: ptr.To("Foo"),
		},
	}, {
		name: "with deps, not happy",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("ReadyReason"),
			Message: ptr.To("ReadyMsg"),
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionTrue,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		}},
		set:   "Bar",
		happy: false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		},
	}, {
		name: "update dep, turns happy",
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		}},
		set:   "Foo",
		happy: false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		},
	}, {
		name: "update dep 1/3, mixed status, still not happy",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		}, {
			Type:    "Baz",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("BazReason"),
			Message: ptr.To("BazMsg"),
		}},
		set:   "Foo",
		happy: false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("BazReason"),
			Message: ptr.To("BazMsg"),
		},
	}, {
		name: "update dep 1/3, unknown status, still not happy",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Bar",
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		}, {
			Type:    "Baz",
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("BazReason"),
			Message: ptr.To("BazMsg"),
		}},
		set:   "Foo",
		happy: false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("BarReason"),
			Message: ptr.To("BarMsg"),
		},
	}, {
		name: "update dep 1/3, unknown status because nil",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}, {
			Type:    "Foo",
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("FooReason"),
			Message: ptr.To("FooMsg"),
		}},
		set:            "Foo",
		conditionTypes: []string{"Foo", "Bar", "Baz"},
		happy:          false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("AwaitingReconciliation"),
			Message: ptr.To("condition \"Bar\" is awaiting reconciliation"),
		},
	}, {
		name: "all happy but not cover all dependents",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("LongStory"),
			Message: ptr.To("Set manually"),
		}, {
			Type:   "Foo",
			Status: metav1.ConditionTrue,
		}},
		set:            "Foo",
		conditionTypes: []string{"Foo", "Bar"}, // dependents is more than conditions.
		happy:          false,
		happyWant: &v1alpha1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionUnknown,
			Reason:  ptr.To("AwaitingReconciliation"),
			Message: ptr.To("condition \"Bar\" is awaiting reconciliation"),
		},
	}, {
		name: "all happy and cover all dependents",
		conditions: []v1alpha1.Condition{{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ptr.To("LongStory"),
			Message: ptr.To("Set manually"),
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
		happyWant: &v1alpha1.Condition{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		},
	}}
	doTestSetTrueAccessor(t, cases)
}

type ConditionSetFalseTest struct {
	name       string
	conditions []v1alpha1.Condition
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

			expected := &v1alpha1.Condition{
				Type:    v1alpha1.ConditionType(tc.set),
				Status:  metav1.ConditionFalse,
				Reason:  ptr.To("UnitTest"),
				Message: ptr.To("calm down, just testing"),
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
		conditions: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
	}, {
		name: "with deps, turns unhappy",
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
	conditions []v1alpha1.Condition
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

			expected := &v1alpha1.Condition{
				Type:    v1alpha1.ConditionType(tc.set),
				Status:  metav1.ConditionUnknown,
				Reason:  ptr.To("UnitTest"),
				Message: ptr.To("idk, just testing"),
			}

			e, a := expected, condSet.For(dut).Get(tc.set)
			if diff := cmp.Diff(e, a, ignoreFields, conditionComparer); diff != "" {
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
		conditions: []v1alpha1.Condition{{
			Type:   ConditionReady,
			Status: metav1.ConditionTrue,
		}},
		set:     ConditionReady,
		unhappy: true,
		happyIs: metav1.ConditionUnknown,
	}, {
		name: "with deps, turns unhappy",
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
		conditions: []v1alpha1.Condition{{
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
