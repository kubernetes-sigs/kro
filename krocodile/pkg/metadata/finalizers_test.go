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

package metadata

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFinalizerOps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		initial          []string
		op               func(obj metav1.Object)
		wantFinalizers   []string
		wantHasFinalizer bool
	}{
		{
			name:             "Has returns false on empty list",
			initial:          nil,
			op:               func(metav1.Object) {},
			wantFinalizers:   nil,
			wantHasFinalizer: false,
		},
		{
			name:             "Set adds when absent",
			initial:          nil,
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Set is a no-op when already present",
			initial:          []string{GraphFinalizer},
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Set preserves unrelated finalizers",
			initial:          []string{"other/finalizer"},
			op:               SetGraphFinalizer,
			wantFinalizers:   []string{"other/finalizer", GraphFinalizer},
			wantHasFinalizer: true,
		},
		{
			name:             "Remove drops the finalizer",
			initial:          []string{GraphFinalizer},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{},
			wantHasFinalizer: false,
		},
		{
			name:             "Remove preserves unrelated finalizers",
			initial:          []string{"first", GraphFinalizer, "third"},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{"first", "third"},
			wantHasFinalizer: false,
		},
		{
			name:             "Remove is a no-op when absent",
			initial:          []string{"other"},
			op:               RemoveGraphFinalizer,
			wantFinalizers:   []string{"other"},
			wantHasFinalizer: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			obj := &metav1.ObjectMeta{Finalizers: append([]string(nil), tc.initial...)}
			tc.op(obj)
			assert.Equal(t, tc.wantFinalizers, obj.Finalizers)
			assert.Equal(t, tc.wantHasFinalizer, HasGraphFinalizer(obj))
		})
	}
}
