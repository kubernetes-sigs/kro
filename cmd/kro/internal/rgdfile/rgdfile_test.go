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

package rgdfile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

const testRGD = `apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: testapps.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: TestApp
`

// The API server rejects a scope outside the CRD's enum, "" included, and,
// with kubectl's strict field validation, unknown fields. Read has to do the
// same, or validate passes RGDs the cluster rejects. An empty group is fine:
// the controller builds the instance CRD in kro.run for it.
func TestRead(t *testing.T) {
	tests := []struct {
		name      string
		schema    string // lines appended to spec.schema
		wantGroup string
		wantScope v1alpha1.ResourceScope
		wantErr   string
	}{
		{
			name:      "group and scope omitted",
			wantGroup: v1alpha1.KRODomainName,
			wantScope: v1alpha1.ResourceScopeNamespaced,
		},
		{
			name:      "group and scope null",
			schema:    "    group:\n    scope: null\n",
			wantGroup: v1alpha1.KRODomainName,
			wantScope: v1alpha1.ResourceScopeNamespaced,
		},
		{
			name:      "group and scope set",
			schema:    "    group: example.com\n    scope: Cluster\n",
			wantGroup: "example.com",
			wantScope: v1alpha1.ResourceScopeCluster,
		},
		{
			name:      "empty group",
			schema:    "    group: \"\"\n",
			wantGroup: v1alpha1.KRODomainName,
			wantScope: v1alpha1.ResourceScopeNamespaced,
		},
		{
			name:    "empty scope",
			schema:  "    scope: \"\"\n",
			wantErr: `invalid ResourceGraphDefinition: spec.schema.scope: Unsupported value: "": supported values: "Namespaced", "Cluster"`,
		},
		{
			name:    "scope outside the enum",
			schema:  "    scope: namespaced\n",
			wantErr: `spec.schema.scope: Unsupported value: "namespaced"`,
		},
		{
			name:      "field added in kro v0.10.0",
			schema:    "    plural: testappz\n",
			wantGroup: v1alpha1.KRODomainName,
			wantScope: v1alpha1.ResourceScopeNamespaced,
		},
		{
			name:    "misspelled group",
			schema:  "    gruop: example.com\n",
			wantErr: `strict decoding error: unknown field "spec.schema.gruop"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd, err := Read(writeFile(t, testRGD+tt.schema))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantGroup, rgd.Spec.Schema.Group)
			assert.Equal(t, tt.wantScope, rgd.Spec.Schema.Scope)
		})
	}
}

// Without a schema there's nothing to default. The graph builder reports it.
func TestReadWithoutSchema(t *testing.T) {
	rgd, err := Read(writeFile(t, "apiVersion: kro.run/v1alpha1\nkind: ResourceGraphDefinition\nspec: {}\n"))
	require.NoError(t, err)
	assert.Nil(t, rgd.Spec.Schema)
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rgd.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}
