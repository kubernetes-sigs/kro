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

// rUID is r with an explicit UID, for the write-ahead/SSA merge cases.
func rUID(nodeID, kind, name, uid string) expv1alpha1.ManagedResource {
	mr := r(nodeID, kind, name)
	mr.UID = uid
	return mr
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
		{
			name: "version-only change: same Group/Kind/ns/name across served versions dedups, no prune",
			// A CRD's v1 and v2 are the SAME stored object. A template bumped
			// from apps/v1 to apps/v2 must NOT prune the old-version entry (which
			// would delete the just-applied object by its stable UID).
			previous: []expv1alpha1.ManagedResource{
				{NodeID: "n", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "ns", Name: "d"},
			},
			applied: []expv1alpha1.ManagedResource{
				{NodeID: "n", APIVersion: "apps/v2", Kind: "Deployment", Namespace: "ns", Name: "d"},
			},
			wantNew: []expv1alpha1.ManagedResource{
				{NodeID: "n", APIVersion: "apps/v2", Kind: "Deployment", Namespace: "ns", Name: "d"},
			},
			wantPruneCandSet: nil,
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

// TestKeyOf_GroupKindIdentity pins that resource identity keys on Group+Kind,
// not the full apiVersion: two served versions of one CRD (same stored object)
// must share a key so a version-only change dedups instead of apply-then-prune.
// The core group (apiVersion without a slash) yields an empty group and must
// still be stable.
func TestKeyOf_GroupKindIdentity(t *testing.T) {
	v1 := expv1alpha1.ManagedResource{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "ns", Name: "d"}
	v2 := expv1alpha1.ManagedResource{APIVersion: "apps/v2", Kind: "Deployment", Namespace: "ns", Name: "d"}
	assert.Equal(t, keyOf(v1), keyOf(v2), "two served versions of one object must share an identity key")

	// Different group is a genuinely different identity.
	other := expv1alpha1.ManagedResource{APIVersion: "extensions/v1", Kind: "Deployment", Namespace: "ns", Name: "d"}
	assert.NotEqual(t, keyOf(v1), keyOf(other), "different groups are distinct identities")

	// Core-group (no slash) is stable and distinct from a grouped kind.
	core := expv1alpha1.ManagedResource{APIVersion: "v1", Kind: "ConfigMap", Namespace: "ns", Name: "c"}
	assert.Equal(t, keyOf(core), keyOf(core))
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
		{
			// Regression: the UID-free intent comes first but must not mask the
			// real UID from the applied set, else Simple.Delete leaks it.
			name: "UID-free intent adopts UID from applied entry",
			previous: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
			},
			applied: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-1"),
			},
			want: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-1"),
			},
		},
		{
			// A UID known in previous survives a UID-free applied entry.
			name: "known UID survives UID-free applied entry",
			previous: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-1"),
			},
			applied: []expv1alpha1.ManagedResource{
				r("n1", "ConfigMap", "a"),
			},
			want: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-1"),
			},
		},
		{
			// delete+recreate: the later (applied) UID is fresher and wins.
			name: "later UID refreshes an earlier UID on recreate",
			previous: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-old"),
			},
			applied: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-new"),
			},
			want: []expv1alpha1.ManagedResource{
				rUID("n1", "ConfigMap", "a", "uid-new"),
			},
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

func TestContributionsAPIRoundTrip(t *testing.T) {
	t.Run("nil and empty are nil-safe", func(t *testing.T) {
		assert.Nil(t, toAPIContributions(nil))
		assert.Nil(t, toAPIContributions([]executor.Contribution{}))
		assert.Nil(t, fromAPIContributions(nil))
		assert.Nil(t, fromAPIContributions([]expv1alpha1.Contribution{}))
	})

	t.Run("round-trips every field including subresource", func(t *testing.T) {
		in := []executor.Contribution{
			{
				APIVersion:   "apps/v1",
				Kind:         "Deployment",
				Namespace:    "ns",
				Name:         "dep",
				Subresource:  "status",
				FieldManager: "fm1",
			},
			{
				// cluster-scoped, main-resource contribution (no ns/subresource)
				APIVersion:   "v1",
				Kind:         "Namespace",
				Name:         "team",
				FieldManager: "fm2",
			},
		}
		api := toAPIContributions(in)
		require.Len(t, api, 2)
		assert.Equal(t, "status", api[0].Subresource)
		assert.Equal(t, "apps/v1", api[0].APIVersion)
		assert.Equal(t, "fm2", api[1].FieldManager)
		assert.Empty(t, api[1].Namespace)

		assert.Equal(t, in, fromAPIContributions(api), "round-trip must be lossless")
	})
}
