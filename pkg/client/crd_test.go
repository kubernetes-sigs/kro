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
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestEnsureUpdatesCRDNamesWithoutVersionChanges(t *testing.T) {
	existing := testCRD()
	desired := existing.DeepCopy()
	desired.Spec.Names.ShortNames = []string{"wd", "widget"}
	desired.Spec.Names.Categories = []string{"kro"}

	wrapper, _ := newTestCRDWrapper(existing)

	require.NoError(t, wrapper.Ensure(context.Background(), *desired, false))

	got, err := wrapper.Get(context.Background(), existing.Name)
	require.NoError(t, err)
	assert.Equal(t, []string{"wd", "widget"}, got.Spec.Names.ShortNames)
	assert.Equal(t, []string{"kro"}, got.Spec.Names.Categories)
}

func TestEnsureClearsCRDNames(t *testing.T) {
	existing := testCRD()
	existing.Spec.Names.ShortNames = []string{"wd", "widget"}
	existing.Spec.Names.Categories = []string{"kro"}
	desired := existing.DeepCopy()
	desired.Spec.Names.ShortNames = nil
	desired.Spec.Names.Categories = nil

	wrapper, _ := newTestCRDWrapper(existing)

	require.NoError(t, wrapper.Ensure(context.Background(), *desired, false))

	got, err := wrapper.Get(context.Background(), existing.Name)
	require.NoError(t, err)
	assert.Empty(t, got.Spec.Names.ShortNames)
	assert.Empty(t, got.Spec.Names.Categories)
}

func TestEnsureReconcilesCRDMetadata(t *testing.T) {
	for _, tt := range []struct {
		name                string
		existingLabels      map[string]string
		desiredLabels       map[string]string
		existingAnnotations map[string]string
		desiredAnnotations  map[string]string
		wantPatches         int
	}{
		{
			name:          "add label",
			desiredLabels: map[string]string{"example.com/ingest": "true"},
			wantPatches:   1,
		},
		{
			name:           "update label",
			existingLabels: map[string]string{"example.com/ingest": "false"},
			desiredLabels:  map[string]string{"example.com/ingest": "true"},
			wantPatches:    1,
		},
		{
			name:               "add annotation",
			desiredAnnotations: map[string]string{"example.com/description": "updated"},
			wantPatches:        1,
		},
		{
			name:                "update annotation",
			existingAnnotations: map[string]string{"example.com/description": "original"},
			desiredAnnotations:  map[string]string{"example.com/description": "updated"},
			wantPatches:         1,
		},
		{
			name:          "add empty label",
			desiredLabels: map[string]string{"example.com/ingest": ""},
			wantPatches:   1,
		},
		{
			name:               "add empty annotation",
			desiredAnnotations: map[string]string{"example.com/description": ""},
			wantPatches:        1,
		},
		{
			name:           "clear label value",
			existingLabels: map[string]string{"example.com/ingest": "true"},
			desiredLabels:  map[string]string{"example.com/ingest": ""},
			wantPatches:    1,
		},
		{
			name:                "clear annotation value",
			existingAnnotations: map[string]string{"example.com/description": "original"},
			desiredAnnotations:  map[string]string{"example.com/description": ""},
			wantPatches:         1,
		},
		{
			name:           "remove label",
			existingLabels: map[string]string{"example.com/ingest": "true"},
			wantPatches:    1,
		},
		{
			name:                "remove annotation with nil desired annotations",
			existingAnnotations: map[string]string{"example.com/description": "original"},
			wantPatches:         1,
		},
		{
			name:                "remove annotation with empty desired annotations",
			existingAnnotations: map[string]string{"example.com/description": "original"},
			desiredAnnotations:  map[string]string{},
			wantPatches:         1,
		},
		{
			name: "remove some metadata while retaining other managed keys",
			existingLabels: map[string]string{
				"example.com/ingest": "true",
				"example.com/keep":   "label",
			},
			desiredLabels: map[string]string{"example.com/keep": "label"},
			existingAnnotations: map[string]string{
				"example.com/description": "original",
				"example.com/keep":        "annotation",
			},
			desiredAnnotations: map[string]string{"example.com/keep": "annotation"},
			wantPatches:        1,
		},
		{
			name: "unchanged metadata with nil annotations",
		},
		{
			name:               "unchanged metadata with empty annotations",
			desiredAnnotations: map[string]string{},
		},
		{
			name:                "unchanged configured metadata",
			existingLabels:      map[string]string{"example.com/ingest": "true"},
			desiredLabels:       map[string]string{"example.com/ingest": "true"},
			existingAnnotations: map[string]string{"example.com/description": ""},
			desiredAnnotations:  map[string]string{"example.com/description": ""},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing := testCRD()
			maps.Copy(existing.Labels, tt.existingLabels)
			existing.Annotations = maps.Clone(tt.existingAnnotations)
			require.NoError(t, trackCRDMetadata(existing))
			desired := testCRD()
			maps.Copy(desired.Labels, tt.desiredLabels)
			desired.Annotations = maps.Clone(tt.desiredAnnotations)
			originalDesired := desired.DeepCopy()

			// Metadata owned by other controllers must be preserved and must not
			// cause otherwise up-to-date CRDs to be patched repeatedly.
			existing.Labels["other.example.com/label"] = "preserved"
			existing.Annotations["other.example.com/annotation"] = "preserved"

			wrapper, clientset := newTestCRDWrapper(existing)
			require.NoError(t, wrapper.Ensure(context.Background(), *desired, false))

			patches := 0
			for _, action := range clientset.Actions() {
				if action.GetVerb() == "patch" {
					patches++
				}
			}
			assert.Equal(t, tt.wantPatches, patches)

			got, err := wrapper.Get(context.Background(), existing.Name)
			require.NoError(t, err)
			wantLabels := maps.Clone(desired.Labels)
			wantLabels["other.example.com/label"] = "preserved"
			wantAnnotations := map[string]string{
				"other.example.com/annotation": "preserved",
				metadata.CRDMetadataAnnotation: got.Annotations[metadata.CRDMetadataAnnotation],
			}
			maps.Copy(wantAnnotations, desired.Annotations)
			assert.Equal(t, wantLabels, got.Labels)
			assert.Equal(t, wantAnnotations, got.Annotations)
			assert.Equal(t, existing.Spec, got.Spec)
			assert.Equal(t, originalDesired, desired, "Ensure must not mutate the caller's CRD")

			var tracked crdMetadataKeys
			require.NoError(t, json.Unmarshal([]byte(got.Annotations[metadata.CRDMetadataAnnotation]), &tracked))
			assert.Equal(t, slices.Sorted(maps.Keys(desired.Labels)), tracked.Labels)
			assert.Equal(t, slices.Sorted(maps.Keys(desired.Annotations)), tracked.Annotations)

			clientset.ClearActions()
			require.NoError(t, wrapper.Ensure(context.Background(), *desired, false))
			for _, action := range clientset.Actions() {
				assert.Equal(t, "get", action.GetVerb(), "reconciliation should be read-only once metadata matches")
			}
		})
	}
}

func TestCRDMergePatchClearsEmptyNames(t *testing.T) {
	patchBytes, err := crdMergePatch(v1.CustomResourceDefinition{}, *testCRD())
	require.NoError(t, err)

	var patch map[string]any
	require.NoError(t, json.Unmarshal(patchBytes, &patch))

	spec := requireMap(t, patch, "spec")
	names := requireMap(t, spec, "names")
	assert.Contains(t, names, "shortNames")
	assert.Nil(t, names["shortNames"])
	assert.Contains(t, names, "categories")
	assert.Nil(t, names["categories"])
	assert.NotContains(t, patch, "status")
	assert.NotContains(t, patch, "apiVersion")
	assert.NotContains(t, patch, "kind")
}

func TestCRDMergePatchIncludesMetadataLabels(t *testing.T) {
	crd := testCRD()
	patchBytes, err := crdMergePatch(v1.CustomResourceDefinition{}, *crd)
	require.NoError(t, err)

	var patch map[string]any
	require.NoError(t, json.Unmarshal(patchBytes, &patch))

	metadataPatch := requireMap(t, patch, "metadata")
	labels := requireMap(t, metadataPatch, "labels")
	assert.Equal(t, "true", labels[metadata.OwnedLabel])
	assert.Equal(t, "widgets", labels[metadata.ResourceGraphDefinitionNameLabel])
	assert.Equal(t, "uid", labels[metadata.ResourceGraphDefinitionIDLabel])
}

func TestCRDMergePatchIncludesResourceVersion(t *testing.T) {
	existing := testCRD()
	existing.ResourceVersion = "42"
	patchBytes, err := crdMergePatch(*existing, *testCRD())
	require.NoError(t, err)

	var patch map[string]any
	require.NoError(t, json.Unmarshal(patchBytes, &patch))
	assert.Equal(t, "42", requireMap(t, patch, "metadata")["resourceVersion"])
}

func requireMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	require.True(t, ok, "%q is not an object", key)
	return value
}

func newTestCRDWrapper(crd *v1.CustomResourceDefinition) (*CRDWrapper, *apiextensionsfake.Clientset) {
	clientset := apiextensionsfake.NewSimpleClientset(crd)
	return newCRDWrapper(CRDWrapperConfig{
		Client:       clientset.ApiextensionsV1(),
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
	}), clientset
}

func testCRD() *v1.CustomResourceDefinition {
	return &v1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "widgets.kro.run",
			Labels: map[string]string{
				metadata.OwnedLabel:                       "true",
				metadata.ResourceGraphDefinitionNameLabel: "widgets",
				metadata.ResourceGraphDefinitionIDLabel:   "uid",
			},
		},
		Spec: v1.CustomResourceDefinitionSpec{
			Group: "kro.run",
			Names: v1.CustomResourceDefinitionNames{
				Plural:   "widgets",
				Singular: "widget",
				Kind:     "Widget",
				ListKind: "WidgetList",
			},
			Scope: v1.NamespaceScoped,
			Versions: []v1.CustomResourceDefinitionVersion{
				{
					Name:    "v1alpha1",
					Served:  true,
					Storage: true,
					Schema: &v1.CustomResourceValidation{
						OpenAPIV3Schema: &v1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]v1.JSONSchemaProps{
								"spec": {Type: "object"},
							},
						},
					},
					Subresources: &v1.CustomResourceSubresources{
						Status: &v1.CustomResourceSubresourceStatus{},
					},
				},
			},
		},
		Status: v1.CustomResourceDefinitionStatus{
			Conditions: []v1.CustomResourceDefinitionCondition{
				{
					Type:   v1.Established,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
}
