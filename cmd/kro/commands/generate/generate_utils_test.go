// Copyright The Kubernetes Authors.
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

package generate

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestMarshalCRDAsKubernetesManifest(t *testing.T) {
	for _, outputFormat := range []string{"yaml", "json"} {
		t.Run(outputFormat, func(t *testing.T) {
			crd := &extv1.CustomResourceDefinition{
				ObjectMeta: metav1.ObjectMeta{Name: "widgets.example.com"},
				Spec: extv1.CustomResourceDefinitionSpec{
					Group: "example.com",
				},
			}
			prepareCRDForOutput(crd)

			manifest, err := marshalObject(crd, outputFormat)
			require.NoError(t, err)

			jsonManifest := manifest
			if outputFormat == "yaml" {
				jsonManifest, err = yaml.YAMLToJSON(manifest)
				require.NoError(t, err)
			}

			var object unstructured.Unstructured
			require.NoError(t, object.UnmarshalJSON(jsonManifest))
			assert.Equal(t, "apiextensions.k8s.io/v1", object.GetAPIVersion())
			assert.Equal(t, "CustomResourceDefinition", object.GetKind())
			assert.Equal(t, "widgets.example.com", object.GetName())
			assert.Equal(t, "dev", object.GetAnnotations()["kro.run/cli-version"])
			group, found, err := unstructured.NestedString(object.Object, "spec", "group")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, "example.com", group)

			var fields map[string]interface{}
			require.NoError(t, json.Unmarshal(jsonManifest, &fields))
			assert.NotContains(t, fields, "typemeta")
			assert.NotContains(t, fields, "objectmeta")
		})
	}
}
