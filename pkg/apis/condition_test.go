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

package apis

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
