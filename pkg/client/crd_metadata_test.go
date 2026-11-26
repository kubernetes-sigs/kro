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

package client

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	clienttesting "k8s.io/client-go/testing"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestEnsureCreatesCRDWithMetadataInventory(t *testing.T) {
	desired := testCRD()
	desired.Labels["example.com/created"] = "label"
	desired.Annotations = map[string]string{"example.com/created": "annotation"}
	originalDesired := desired.DeepCopy()

	wrapper, clientset := newTestCRDWrapper(desired)
	// Start without a CRD. The fake client preserves the fixture's Established
	// status on Create, so Ensure can also complete its readiness check.
	require.NoError(t, clientset.Tracker().Delete(
		v1.SchemeGroupVersion.WithResource("customresourcedefinitions"), "", desired.Name,
	))

	require.NoError(t, wrapper.Ensure(t.Context(), *desired, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions(), "create")
	got, err := wrapper.Get(t.Context(), desired.Name)
	require.NoError(t, err)
	assert.Equal(t, desired.Labels, got.Labels)
	assert.Equal(t, "annotation", got.Annotations["example.com/created"])
	assert.Equal(t, crdMetadataKeys{
		Labels:      slices.Sorted(maps.Keys(desired.Labels)),
		Annotations: []string{"example.com/created"},
	}, readCRDMetadataInventory(t, got))
	assert.Equal(t, originalDesired, desired, "Ensure must not modify its caller's CRD")

	clientset.ClearActions()
	withoutMetadata := testCRD()
	require.NoError(t, wrapper.Ensure(t.Context(), *withoutMetadata, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions(), "patch")
	got, err = wrapper.Get(t.Context(), desired.Name)
	require.NoError(t, err)
	assert.Equal(t, withoutMetadata.Labels, got.Labels)
	assert.NotContains(t, got.Annotations, "example.com/created")
	assert.Len(t, got.Annotations, 1, "only the controller inventory should remain")
	assert.Equal(t, crdMetadataKeys{
		Labels: slices.Sorted(maps.Keys(withoutMetadata.Labels)),
	}, readCRDMetadataInventory(t, got))
	assert.Equal(t, desired.Spec, got.Spec)

	clientset.ClearActions()
	require.NoError(t, wrapper.Ensure(t.Context(), *withoutMetadata, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions())
}

func TestEnsureBootstrapsLegacyCRDMetadata(t *testing.T) {
	desired := testCRD()
	desired.Labels["example.com/managed"] = "label"
	desired.Annotations = map[string]string{"example.com/managed": "annotation"}
	originalDesired := desired.DeepCopy()

	existing := desired.DeepCopy()
	existing.Labels["legacy.example.com/unknown"] = "legacy label"
	existing.Annotations["legacy.example.com/unknown"] = "legacy annotation"
	require.NotContains(t, existing.Annotations, metadata.CRDMetadataAnnotation)

	wrapper, clientset := newTestCRDWrapper(existing)
	// All desired values already match: the inventory alone must cause a patch.
	require.NoError(t, wrapper.Ensure(t.Context(), *desired, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions(), "patch")
	got, err := wrapper.Get(t.Context(), desired.Name)
	require.NoError(t, err)
	assert.Equal(t, existing.Labels, got.Labels)
	assert.Equal(t, "annotation", got.Annotations["example.com/managed"])
	assert.Equal(t, "legacy annotation", got.Annotations["legacy.example.com/unknown"])
	assert.Equal(t, crdMetadataKeys{
		Labels:      slices.Sorted(maps.Keys(desired.Labels)),
		Annotations: []string{"example.com/managed"},
	}, readCRDMetadataInventory(t, got))
	assert.Equal(t, originalDesired, desired)

	clientset.ClearActions()
	require.NoError(t, wrapper.Ensure(t.Context(), *desired, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions())

	// Use a fresh wrapper and client seeded only from persisted state: pruning
	// must not depend on the wrapper that performed legacy adoption.
	wrapper, clientset = newTestCRDWrapper(got)
	withoutMetadata := testCRD()
	require.NoError(t, wrapper.Ensure(t.Context(), *withoutMetadata, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions(), "patch")
	got, err = wrapper.Get(t.Context(), desired.Name)
	require.NoError(t, err)
	wantLabels := maps.Clone(withoutMetadata.Labels)
	wantLabels["legacy.example.com/unknown"] = "legacy label"
	assert.Equal(t, wantLabels, got.Labels)
	assert.Equal(t, map[string]string{
		"legacy.example.com/unknown":   "legacy annotation",
		metadata.CRDMetadataAnnotation: got.Annotations[metadata.CRDMetadataAnnotation],
	}, got.Annotations)
	assert.Equal(t, crdMetadataKeys{
		Labels: slices.Sorted(maps.Keys(withoutMetadata.Labels)),
	}, readCRDMetadataInventory(t, got))
	assert.Equal(t, existing.Spec, got.Spec)

	clientset.ClearActions()
	require.NoError(t, wrapper.Ensure(t.Context(), *withoutMetadata, false))
	requireCRDMetadataWriteVerbs(t, clientset.Actions())
}

func TestEnsureRejectsMalformedCRDMetadataInventory(t *testing.T) {
	for _, tt := range []struct {
		name      string
		inventory string
	}{
		{name: "empty value", inventory: ""},
		{name: "invalid JSON", inventory: "not-json"},
		{name: "truncated JSON", inventory: `{"labels":`},
		{name: "array instead of object", inventory: `[]`},
		{name: "invalid label keys", inventory: `{"labels":"example.com/managed"}`},
		{name: "invalid annotation keys", inventory: `{"annotations":[1]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing := testCRD()
			existing.Labels["example.com/managed"] = "unchanged"
			existing.Annotations = map[string]string{
				metadata.CRDMetadataAnnotation: tt.inventory,
				"example.com/managed":          "unchanged",
			}
			desired := testCRD()
			desired.Annotations = map[string]string{"example.com/new": "not applied"}
			originalDesired := desired.DeepCopy()
			wrapper, clientset := newTestCRDWrapper(existing)

			err := wrapper.Ensure(t.Context(), *desired, false)
			require.ErrorContains(t, err, "failed to read previous CRD metadata keys")
			requireCRDMetadataWriteVerbs(t, clientset.Actions())
			got, err := wrapper.Get(t.Context(), existing.Name)
			require.NoError(t, err)
			assert.Equal(t, existing, got, "malformed inventory must leave the CRD untouched")
			assert.Equal(t, originalDesired, desired)
		})
	}
}

func TestTrackCRDMetadataPreservesCallerAnnotations(t *testing.T) {
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		wantKeys    []string
	}{
		{name: "nil annotations"},
		{name: "empty annotations", annotations: map[string]string{}},
		{
			name: "configured annotations",
			annotations: map[string]string{
				"z.example.com/annotation": "last",
				"a.example.com/annotation": "",
			},
			wantKeys: []string{"a.example.com/annotation", "z.example.com/annotation"},
		},
		{
			name: "user supplied inventory",
			annotations: map[string]string{
				metadata.CRDMetadataAnnotation: `{"annotations":["external.example.com/forged"]}`,
				"example.com/annotation":       "preserved",
			},
			wantKeys: []string{"example.com/annotation"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			caller := testCRD()
			caller.Labels["z.example.com/label"] = "last"
			caller.Labels["a.example.com/label"] = "first"
			caller.Annotations = tt.annotations
			originalCaller := caller.DeepCopy()
			// Ensure receives a value copy, which still aliases the caller's maps.
			tracked := *caller

			require.NoError(t, trackCRDMetadata(&tracked))
			assert.Equal(t, originalCaller, caller)
			assert.Equal(t, crdMetadataKeys{
				Labels: []string{
					"a.example.com/label",
					metadata.OwnedLabel,
					metadata.ResourceGraphDefinitionIDLabel,
					metadata.ResourceGraphDefinitionNameLabel,
					"z.example.com/label",
				},
				Annotations: tt.wantKeys,
			}, readCRDMetadataInventory(t, &tracked))
			assert.Len(t, tracked.Annotations, len(tt.wantKeys)+1)
			for _, key := range tt.wantKeys {
				assert.Equal(t, tt.annotations[key], tracked.Annotations[key])
			}

			inventory := tracked.Annotations[metadata.CRDMetadataAnnotation]
			require.NoError(t, trackCRDMetadata(&tracked))
			assert.Equal(t, inventory, tracked.Annotations[metadata.CRDMetadataAnnotation],
				"repeated tracking must not inventory the tracker itself")
			assert.Equal(t, originalCaller, caller)
		})
	}
}

func readCRDMetadataInventory(t *testing.T, crd *v1.CustomResourceDefinition) crdMetadataKeys {
	t.Helper()
	require.Contains(t, crd.Annotations, metadata.CRDMetadataAnnotation)
	var keys crdMetadataKeys
	require.NoError(t, json.Unmarshal([]byte(crd.Annotations[metadata.CRDMetadataAnnotation]), &keys))
	return keys
}

func requireCRDMetadataWriteVerbs(t *testing.T, actions []clienttesting.Action, want ...string) {
	t.Helper()
	var writes []string
	for _, action := range actions {
		if action.GetVerb() != "get" {
			writes = append(writes, action.GetVerb())
		}
	}
	require.Equal(t, want, writes)
}
