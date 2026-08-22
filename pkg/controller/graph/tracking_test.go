// Copyright 2026 The Kubernetes Authors.
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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

// helper to keep table rows readable.
func r(nodeID, kind, name string) expv1alpha1.ManagedResource {
	return expv1alpha1.ManagedResource{
		NodeID:     nodeID,
		APIVersion: "v1",
		Kind:       kind,
		Namespace:  "ns",
		Name:       name,
	}
}

// TestDiffManagedResources walks the key shapes the reconciler relies
// on. The contract: newSet = Applied ∪ entries preserved from previous
// because their NodeID is Unresolved; pruneCandidates = previous \ that.
func TestDiffManagedResources(t *testing.T) {
	tests := []struct {
		name             string
		previous         []expv1alpha1.ManagedResource
		applied          []expv1alpha1.ManagedResource
		unresolved       []string
		wantNew          []expv1alpha1.ManagedResource
		wantPruneCandSet []expv1alpha1.ManagedResource
	}{
		{
			name:             "empty previous + empty applied → all empty",
			wantNew:          []expv1alpha1.ManagedResource{},
			wantPruneCandSet: nil,
		},
		{
			name:             "first reconcile: nothing previous, all applied → newSet equals applied",
			applied:          []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "cm-1")},
			wantNew:          []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "cm-1")},
			wantPruneCandSet: nil,
		},
		{
			name:             "stable reapply: previous == applied → no prune",
			previous:         []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "cm-1")},
			applied:          []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "cm-1")},
			wantNew:          []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "cm-1")},
			wantPruneCandSet: nil,
		},
		{
			name: "rename: previous entry not in applied → prune candidate",
			previous: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "old-name"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "new-name"),
			},
			wantNew: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "new-name"),
			},
			wantPruneCandSet: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "old-name"),
			},
		},
		{
			name: "forEach shrunk: 3 instances → 2 → one prune candidate",
			previous: []expv1alpha1.ManagedResource{
				r("n", "ConfigMap", "cm-a"),
				r("n", "ConfigMap", "cm-b"),
				r("n", "ConfigMap", "cm-c"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n", "ConfigMap", "cm-a"),
				r("n", "ConfigMap", "cm-b"),
			},
			wantNew: []expv1alpha1.ManagedResource{
				r("n", "ConfigMap", "cm-a"),
				r("n", "ConfigMap", "cm-b"),
			},
			wantPruneCandSet: []expv1alpha1.ManagedResource{
				r("n", "ConfigMap", "cm-c"),
			},
		},
		{
			name: "includeWhen flipped to false: node not in applied, not unresolved → prune",
			previous: []expv1alpha1.ManagedResource{
				r("n-flag", "ConfigMap", "guarded"),
			},
			applied:          nil,
			wantNew:          []expv1alpha1.ManagedResource{},
			wantPruneCandSet: []expv1alpha1.ManagedResource{r("n-flag", "ConfigMap", "guarded")},
		},
		{
			name: "unresolved nodeID: previous entry preserved (not pruned)",
			previous: []expv1alpha1.ManagedResource{
				r("n-pending", "ConfigMap", "still-want-this"),
			},
			applied:          nil,
			unresolved:       []string{"n-pending"},
			wantNew:          []expv1alpha1.ManagedResource{r("n-pending", "ConfigMap", "still-want-this")},
			wantPruneCandSet: nil,
		},
		{
			name: "mixed: some applied, one renamed, one unresolved",
			previous: []expv1alpha1.ManagedResource{
				r("n-stable", "ConfigMap", "stable"),
				r("n-renamed", "ConfigMap", "old"),
				r("n-pending", "ConfigMap", "uncertain"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n-stable", "ConfigMap", "stable"),
				r("n-renamed", "ConfigMap", "new"),
			},
			unresolved: []string{"n-pending"},
			wantNew: []expv1alpha1.ManagedResource{
				r("n-stable", "ConfigMap", "stable"),
				r("n-renamed", "ConfigMap", "new"),
				r("n-pending", "ConfigMap", "uncertain"),
			},
			wantPruneCandSet: []expv1alpha1.ManagedResource{
				r("n-renamed", "ConfigMap", "old"),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			newSet, pruneCandidates := diffManagedResources(tc.previous, executor.ApplyResult{
				Applied:    tc.applied,
				Unresolved: tc.unresolved,
			})
			assert.ElementsMatch(t, tc.wantNew, newSet, "newSet")
			assert.ElementsMatch(t, tc.wantPruneCandSet, pruneCandidates, "pruneCandidates")
		})
	}
}

// TestUnionManagedResources covers the soft/hard-error path: instead of
// pruning, the reconciler widens status to cover both previous and
// applied. Dedup must be by identity (NodeID + GVKNN), not pointer.
func TestUnionManagedResources(t *testing.T) {
	tests := []struct {
		name     string
		previous []expv1alpha1.ManagedResource
		applied  []expv1alpha1.ManagedResource
		want     []expv1alpha1.ManagedResource
	}{
		{
			name: "no duplicates: previous then applied",
			previous: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n2", "ConfigMap", "b"),
			},
			want: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n2", "ConfigMap", "b"),
			},
		},
		{
			name: "duplicates skipped on second occurrence",
			previous: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n2", "ConfigMap", "b"),
			},
			want: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
				r("n2", "ConfigMap", "b"),
			},
		},
		{
			name:    "empty previous returns applied",
			applied: []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "a")},
			want:    []expv1alpha1.ManagedResource{r("n1", "ConfigMap", "a")},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unionManagedResources(tc.previous, tc.applied)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestReadContributions(t *testing.T) {
	t.Run("nil object or nil annotations returns nil", func(t *testing.T) {
		res, err := ReadContributions(nil)
		require.NoError(t, err)
		assert.Nil(t, res)

		g := &expv1alpha1.Graph{}
		res, err = ReadContributions(g)
		require.NoError(t, err)
		assert.Nil(t, res)
	})

	t.Run("empty annotation returns nil", func(t *testing.T) {
		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					metadata.PatchContributionsAnnotation: "",
				},
			},
		}
		res, err := ReadContributions(g)
		require.NoError(t, err)
		assert.Nil(t, res)
	})

	t.Run("valid JSON returns contributions", func(t *testing.T) {
		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					metadata.PatchContributionsAnnotation: `[{"apiVersion":"v1","kind":"ConfigMap","namespace":"ns","name":"cm","fieldManager":"fm1"}]`,
				},
			},
		}
		res, err := ReadContributions(g)
		require.NoError(t, err)
		require.Len(t, res, 1)
		assert.Equal(t, "ConfigMap", res[0].Kind)
		assert.Equal(t, "cm", res[0].Name)
		assert.Equal(t, "fm1", res[0].FieldManager)
	})

	t.Run("malformed JSON returns error", func(t *testing.T) {
		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					metadata.PatchContributionsAnnotation: `not-valid-json`,
				},
			},
		}
		_, err := ReadContributions(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unmarshal patch contributions")
	})
}

func TestMarshalContributions(t *testing.T) {
	t.Run("empty or nil returns empty string", func(t *testing.T) {
		raw, err := MarshalContributions(nil)
		require.NoError(t, err)
		assert.Equal(t, "", raw)

		raw, err = MarshalContributions([]executor.Contribution{})
		require.NoError(t, err)
		assert.Equal(t, "", raw)
	})

	t.Run("non-empty returns JSON", func(t *testing.T) {
		contribs := []executor.Contribution{
			{
				APIVersion:   "v1",
				Kind:         "ConfigMap",
				Namespace:    "ns",
				Name:         "cm",
				FieldManager: "fm1",
			},
		}
		raw, err := MarshalContributions(contribs)
		require.NoError(t, err)
		assert.Contains(t, raw, `"kind":"ConfigMap"`)
		assert.Contains(t, raw, `"fieldManager":"fm1"`)
	})
}

func TestDiffContributions(t *testing.T) {
	c1 := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cm1", FieldManager: "fm1"}
	c2 := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cm2", FieldManager: "fm2"}
	c3 := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cm3", FieldManager: "fm3"}

	t.Run("prior entry missing in current is released", func(t *testing.T) {
		released := DiffContributions([]executor.Contribution{c1, c2}, []executor.Contribution{c1})
		assert.Equal(t, []executor.Contribution{c2}, released)
	})

	t.Run("all prior entries kept in current yields empty released", func(t *testing.T) {
		released := DiffContributions([]executor.Contribution{c1}, []executor.Contribution{c1, c3})
		assert.Empty(t, released)
	})
}

func TestUnionContributions(t *testing.T) {
	c1 := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cm1", FieldManager: "fm1"}
	c2 := executor.Contribution{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "cm2", FieldManager: "fm2"}

	t.Run("deduplicates identical contributions", func(t *testing.T) {
		union := UnionContributions([]executor.Contribution{c1, c2}, []executor.Contribution{c1})
		assert.Equal(t, []executor.Contribution{c1, c2}, union)
	})

	t.Run("combines disjoint contributions", func(t *testing.T) {
		union := UnionContributions([]executor.Contribution{c1}, []executor.Contribution{c2})
		assert.Equal(t, []executor.Contribution{c1, c2}, union)
	})
}
