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

package applyset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
)

func validInventoryParent() *testParent {
	parent := newTestParent(schema.GroupVersionKind{
		Group: "kro.run", Version: "v1alpha1", Kind: "TestKind",
	})
	metadata := Metadata{
		ID:                   ID(parent),
		Tooling:              "kro/v1.0.0",
		GroupKinds:           sets.New(schema.GroupKind{Kind: "ConfigMap"}),
		AdditionalNamespaces: sets.New("other-ns"),
	}
	parent.SetLabels(metadata.Labels())
	parent.SetAnnotations(metadata.Annotations())
	return parent
}

func TestValidateParentInventory(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testParent)
		wantError string
	}{
		{name: "valid"},
		{
			name: "legacy parent without hash",
			mutate: func(parent *testParent) {
				delete(parent.Annotations, ApplySetInventoryHashAnnotation)
			},
		},
		{
			name: "wrong parent ID",
			mutate: func(parent *testParent) {
				parent.Labels[ApplySetParentIDLabel] = "applyset-wrong-v1"
			},
			wantError: ApplySetParentIDLabel,
		},
		{
			name: "foreign tooling",
			mutate: func(parent *testParent) {
				parent.Annotations[ApplySetToolingAnnotation] = "other/v1"
			},
			wantError: ApplySetToolingAnnotation,
		},
		{
			name: "missing group kinds",
			mutate: func(parent *testParent) {
				delete(parent.Annotations, ApplySetGKsAnnotation)
			},
			wantError: ApplySetGKsAnnotation,
		},
		{
			name: "missing namespaces",
			mutate: func(parent *testParent) {
				delete(parent.Annotations, ApplySetAdditionalNamespacesAnnotation)
			},
			wantError: ApplySetAdditionalNamespacesAnnotation,
		},
		{
			name: "malformed group kinds",
			mutate: func(parent *testParent) {
				parent.Annotations[ApplySetGKsAnnotation] = "ConfigMap,"
			},
			wantError: ApplySetGKsAnnotation,
		},
		{
			name: "malformed namespace",
			mutate: func(parent *testParent) {
				parent.Annotations[ApplySetAdditionalNamespacesAnnotation] = "NOT_A_NAMESPACE"
			},
			wantError: ApplySetAdditionalNamespacesAnnotation,
		},
		{
			name: "inventory hash mismatch",
			mutate: func(parent *testParent) {
				parent.Annotations[ApplySetGKsAnnotation] = "ConfigMap,Secret"
			},
			wantError: ApplySetInventoryHashAnnotation,
		},
		{
			name: "explicit empty inventory",
			mutate: func(parent *testParent) {
				metadata := Metadata{ID: ID(parent), Tooling: "kro/v1.0.0"}
				parent.SetLabels(metadata.Labels())
				parent.SetAnnotations(metadata.Annotations())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := validInventoryParent()
			if tt.mutate != nil {
				tt.mutate(parent)
			}
			err := ValidateParentInventory(parent)
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantError)
		})
	}
}
