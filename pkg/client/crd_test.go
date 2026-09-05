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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

func TestEnsureUpdatesCRDNamesWithoutVersionChanges(t *testing.T) {
	existing := testCRD()
	desired := existing.DeepCopy()
	desired.Spec.Names.ShortNames = []string{"wd", "widget"}
	desired.Spec.Names.Categories = []string{"kro"}

	wrapper := newTestCRDWrapper(existing)

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

	wrapper := newTestCRDWrapper(existing)

	require.NoError(t, wrapper.Ensure(context.Background(), *desired, false))

	got, err := wrapper.Get(context.Background(), existing.Name)
	require.NoError(t, err)
	assert.Empty(t, got.Spec.Names.ShortNames)
	assert.Empty(t, got.Spec.Names.Categories)
}

func TestEnsureBreakingChangeErrorMentionsAnnotation(t *testing.T) {
	existing := testCRD()
	desired := existing.DeepCopy()
	desired.Spec.Versions[0].Served = false

	wrapper := newTestCRDWrapper(existing)

	err := wrapper.Ensure(context.Background(), *desired, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), v1alpha1.AllowBreakingChangesAnnotation)
	assert.Contains(t, err.Error(), "breaking changes detected")
}

func TestCRDMergePatchClearsEmptyNames(t *testing.T) {
	patchBytes, err := crdMergePatch(*testCRD())
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
	patchBytes, err := crdMergePatch(*crd)
	require.NoError(t, err)

	var patch map[string]any
	require.NoError(t, json.Unmarshal(patchBytes, &patch))

	metadataPatch := requireMap(t, patch, "metadata")
	labels := requireMap(t, metadataPatch, "labels")
	assert.Equal(t, "true", labels[metadata.OwnedLabel])
	assert.Equal(t, "widgets", labels[metadata.ResourceGraphDefinitionNameLabel])
	assert.Equal(t, "uid", labels[metadata.ResourceGraphDefinitionIDLabel])
}

func requireMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	require.True(t, ok, "%q is not an object", key)
	return value
}

func newTestCRDWrapper(crd *v1.CustomResourceDefinition) *CRDWrapper {
	clientset := apiextensionsfake.NewSimpleClientset(crd)
	return newCRDWrapper(CRDWrapperConfig{
		Client:       clientset.ApiextensionsV1(),
		PollInterval: time.Millisecond,
		Timeout:      time.Second,
	})
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
