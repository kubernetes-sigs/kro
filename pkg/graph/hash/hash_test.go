// Copyright 2025 The Kubernetes Authors.
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

package hash

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
)

func TestSpecCases(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "equivalent ordering produces the same hash",
			run: func(t *testing.T) {
				specA, specB := equivalentOrderingSpecs()

				hashA, err := Spec(specA)
				require.NoError(t, err)
				hashB, err := Spec(specB)
				require.NoError(t, err)

				normA, err := normalizeSpec(specA)
				require.NoError(t, err)
				normB, err := normalizeSpec(specB)
				require.NoError(t, err)
				normAJSON, err := json.Marshal(normA)
				require.NoError(t, err)
				normBJSON, err := json.Marshal(normB)
				require.NoError(t, err)

				assert.JSONEq(t, string(normAJSON), string(normBJSON))
				assert.Equal(t, hashA, hashB)
			},
		},
		{
			name: "semantic template changes produce a different hash",
			run: func(t *testing.T) {
				base := deploymentSpec()
				changed := *base.DeepCopy()
				changed.Resources[0].Template = raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app"},"spec":{"replicas":"${schema.spec.replicas + 1}"}}`)

				assertDifferentHashes(t, base, changed)
			},
		},
		{
			name: "readyWhen contributes to the hash",
			run: func(t *testing.T) {
				base := conditionalSpec()
				changed := *base.DeepCopy()
				changed.Resources[0].ReadyWhen = []string{"self.status.readyReplicas > 1"}

				assertDifferentHashes(t, base, changed)
			},
		},
		{
			name: "includeWhen contributes to the hash",
			run: func(t *testing.T) {
				base := conditionalSpec()
				changed := *base.DeepCopy()
				changed.Resources[0].IncludeWhen = []string{"schema.spec.enabled == false"}

				assertDifferentHashes(t, base, changed)
			},
		},
		{
			name: "forEach contributes to the hash",
			run: func(t *testing.T) {
				base := conditionalSpec()
				changed := *base.DeepCopy()
				changed.Resources[0].ForEach = []v1alpha1.ForEachDimension{{"region": "${schema.spec.otherRegions}"}}

				assertDifferentHashes(t, base, changed)
			},
		},
		{
			name: "readyWhen/includeWhen order does not affect the hash",
			run: func(t *testing.T) {
				specA := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{{
						ID:          "svc",
						Template:    raw(`{"apiVersion":"v1","kind":"Service"}`),
						ReadyWhen:   []string{"a", "b", "c"},
						IncludeWhen: []string{"x", "y"},
					}},
				}
				specB := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{{
						ID:          "svc",
						Template:    raw(`{"apiVersion":"v1","kind":"Service"}`),
						ReadyWhen:   []string{"c", "a", "b"},
						IncludeWhen: []string{"y", "x"},
					}},
				}

				hashA, err := Spec(specA)
				require.NoError(t, err)
				hashB, err := Spec(specB)
				require.NoError(t, err)
				assert.Equal(t, hashA, hashB)
			},
		},
		{
			name: "forEach order affects the hash",
			run: func(t *testing.T) {
				specA := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{{
						ID:       "svc",
						Template: raw(`{"apiVersion":"v1","kind":"ConfigMap"}`),
						ForEach: []v1alpha1.ForEachDimension{
							{"region": "${schema.spec.regions}"},
							{"zone": "${schema.spec.zones}"},
						},
					}},
				}
				specB := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{{
						ID:       "svc",
						Template: raw(`{"apiVersion":"v1","kind":"ConfigMap"}`),
						ForEach: []v1alpha1.ForEachDimension{
							{"zone": "${schema.spec.zones}"},
							{"region": "${schema.spec.regions}"},
						},
					}},
				}

				assertDifferentHashes(t, specA, specB)
			},
		},
		{
			name: "resource list order does not affect the hash",
			run: func(t *testing.T) {
				first := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{
						{ID: "a", Template: raw(`{"kind":"ConfigMap","apiVersion":"v1"}`)},
						{ID: "b", Template: raw(`{"kind":"Service","apiVersion":"v1"}`)},
					},
				}
				second := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
					Resources: []*v1alpha1.Resource{
						{ID: "b", Template: raw(`{"kind":"Service","apiVersion":"v1"}`)},
						{ID: "a", Template: raw(`{"kind":"ConfigMap","apiVersion":"v1"}`)},
					},
				}

				hashA, err := Spec(first)
				require.NoError(t, err)
				hashB, err := Spec(second)
				require.NoError(t, err)
				assert.Equal(t, hashA, hashB)
			},
		},
		{
			name: "raw extension object is normalized when raw is empty",
			run: func(t *testing.T) {
				spec := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{
						Kind:       "App",
						APIVersion: "v1alpha1",
						Spec: runtime.RawExtension{
							Object: &unstructured.Unstructured{Object: map[string]any{"replicas": "integer", "image": "string"}},
						},
					},
				}

				h, err := Spec(spec)
				require.NoError(t, err)
				assert.NotEmpty(t, h)
			},
		},
		{
			name: "invalid schema raw extension returns an error",
			run: func(t *testing.T) {
				spec := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1", Spec: raw(`{"broken":`)},
				}

				_, err := Spec(spec)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "schema.spec")
			},
		},
		{
			name: "marshal failures are returned",
			run: func(t *testing.T) {
				withJSONMarshalStub(t, func(any) ([]byte, error) {
					return nil, errors.New("boom")
				})

				_, err := Spec(v1alpha1.ResourceGraphDefinitionSpec{})
				require.Error(t, err)
				assert.Contains(t, err.Error(), "marshal normalized spec")
			},
		},
		{
			name: "hashing is deterministic",
			run: func(t *testing.T) {
				spec := v1alpha1.ResourceGraphDefinitionSpec{
					Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1", Group: "kro.run", Spec: raw(`{"name":"string","replicas":"integer"}`)},
				}

				first, err := Spec(spec)
				require.NoError(t, err)
				second, err := Spec(spec)
				require.NoError(t, err)
				assert.Equal(t, first, second)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t)
		})
	}
}

func TestNormalizeSpecCases(t *testing.T) {
	tests := []struct {
		name            string
		spec            v1alpha1.ResourceGraphDefinitionSpec
		wantErrContains string
		assertResult    func(*testing.T, v1alpha1.ResourceGraphDefinitionSpec)
	}{
		{
			name: "invalid schema types returns an error",
			spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1", Types: raw(`{"broken":`)},
			},
			wantErrContains: "normalize schema.types",
		},
		{
			name: "invalid schema status returns an error",
			spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1", Status: raw(`{"broken":`)},
			},
			wantErrContains: "normalize schema.status",
		},
		{
			name: "nil resources are skipped while other templates are normalized",
			spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
				Resources: []*v1alpha1.Resource{
					nil,
					{ID: "service", Template: raw(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"my-svc"}}`)},
				},
			},
			assertResult: func(t *testing.T, normalized v1alpha1.ResourceGraphDefinitionSpec) {
				require.Len(t, normalized.Resources, 2)
				assert.Nil(t, normalized.Resources[0])
				assert.JSONEq(t, `{"apiVersion":"v1","kind":"Service","metadata":{"name":"my-svc"}}`, string(normalized.Resources[1].Template.Raw))
			},
		},
		{
			name: "invalid resource template returns an error",
			spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema:    &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
				Resources: []*v1alpha1.Resource{{ID: "service", Template: raw(`{"broken":`)}},
			},
			wantErrContains: "normalize resources[0].template",
		},
		{
			name: "forEach order is preserved while condition ordering is normalized",
			spec: v1alpha1.ResourceGraphDefinitionSpec{
				Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
				Resources: []*v1alpha1.Resource{{
					ID:          "service",
					Template:    raw(`{"apiVersion":"v1","kind":"ConfigMap"}`),
					ReadyWhen:   []string{"b", "a"},
					IncludeWhen: []string{"y", "x"},
					ForEach: []v1alpha1.ForEachDimension{
						{"zone": "${schema.spec.zones}"},
						{"region": "${schema.spec.regions}"},
					},
				}},
			},
			assertResult: func(t *testing.T, normalized v1alpha1.ResourceGraphDefinitionSpec) {
				require.Len(t, normalized.Resources, 1)
				assert.Equal(t, []string{"a", "b"}, normalized.Resources[0].ReadyWhen)
				assert.Equal(t, []string{"x", "y"}, normalized.Resources[0].IncludeWhen)
				assert.Equal(t, []v1alpha1.ForEachDimension{
					{"zone": "${schema.spec.zones}"},
					{"region": "${schema.spec.regions}"},
				}, normalized.Resources[0].ForEach)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalized, err := normalizeSpec(tt.spec)
			if tt.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContains)
				return
			}

			require.NoError(t, err)
			if tt.assertResult != nil {
				tt.assertResult(t, normalized)
			}
		})
	}
}

func TestNormalizeRawExtensionCases(t *testing.T) {
	tests := []struct {
		name            string
		ext             runtime.RawExtension
		wantErrContains string
		assertResult    func(*testing.T, runtime.RawExtension)
	}{
		{
			name: "empty raw extension stays empty",
			ext:  runtime.RawExtension{},
			assertResult: func(t *testing.T, normalized runtime.RawExtension) {
				assert.Empty(t, normalized.Raw)
				assert.Nil(t, normalized.Object)
			},
		},
		{
			name:            "object marshal failures are returned",
			ext:             runtime.RawExtension{Object: badRuntimeObject{}},
			wantErrContains: "marshal raw extension object",
		},
		{
			name:            "canonical payload marshal failures are returned",
			ext:             raw(`{"value":"ok"}`),
			wantErrContains: "marshal canonical raw extension payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "canonical payload marshal failures are returned" {
				withJSONMarshalStub(t, func(v any) ([]byte, error) {
					switch v.(type) {
					case map[string]any:
						return nil, errors.New("boom")
					default:
						return json.Marshal(v)
					}
				})
			}

			normalized, err := normalizeRawExtension(tt.ext)
			if tt.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContains)
				return
			}

			require.NoError(t, err)
			if tt.assertResult != nil {
				tt.assertResult(t, normalized)
			}
		})
	}
}

func equivalentOrderingSpecs() (v1alpha1.ResourceGraphDefinitionSpec, v1alpha1.ResourceGraphDefinitionSpec) {
	return v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				Kind:       "App",
				APIVersion: "v1alpha1",
				Group:      "kro.run",
				Spec:       raw(`{"b":{"x":2,"a":1},"a":[{"k":"v","n":1}]}`),
				Types:      raw(`{"Server":{"port":"integer","host":"string"}}`),
				Status:     raw(`{"endpoint":"${service.status.loadBalancer.ingress[0].hostname}"}`),
				Metadata: &v1alpha1.CRDMetadata{
					Labels:      map[string]string{"z": "1", "a": "2"},
					Annotations: map[string]string{"team": "kro"},
				},
			},
			Resources: []*v1alpha1.Resource{{
				ID:       "service",
				Template: raw(`{"apiVersion":"v1","kind":"Service","metadata":{"labels":{"y":"2","x":"1"},"name":"my-svc"},"spec":{"ports":[{"port":80}]}}`),
			}},
		}, v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				Kind:       "App",
				APIVersion: "v1alpha1",
				Group:      "kro.run",
				Spec:       raw(`{"a":[{"n":1,"k":"v"}],"b":{"a":1,"x":2}}`),
				Types:      raw(`{"Server":{"host":"string","port":"integer"}}`),
				Status:     raw(`{"endpoint":"${service.status.loadBalancer.ingress[0].hostname}"}`),
				Metadata: &v1alpha1.CRDMetadata{
					Labels:      map[string]string{"a": "2", "z": "1"},
					Annotations: map[string]string{"team": "kro"},
				},
			},
			Resources: []*v1alpha1.Resource{{
				ID:       "service",
				Template: raw(`{"kind":"Service","apiVersion":"v1","metadata":{"name":"my-svc","labels":{"x":"1","y":"2"}},"spec":{"ports":[{"port":80}]}}`),
			}},
		}
}

func deploymentSpec() v1alpha1.ResourceGraphDefinitionSpec {
	return v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1", Group: "kro.run", Spec: raw(`{"replicas":"integer"}`)},
		Resources: []*v1alpha1.Resource{{
			ID:       "deployment",
			Template: raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app"},"spec":{"replicas":"${schema.spec.replicas}"}}`),
		}},
	}
}

func conditionalSpec() v1alpha1.ResourceGraphDefinitionSpec {
	return v1alpha1.ResourceGraphDefinitionSpec{
		Schema: &v1alpha1.Schema{Kind: "App", APIVersion: "v1alpha1"},
		Resources: []*v1alpha1.Resource{{
			ID:          "deployment",
			Template:    raw(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"app"}}`),
			ReadyWhen:   []string{"self.status.readyReplicas > 0"},
			IncludeWhen: []string{"schema.spec.enabled == true"},
			ForEach:     []v1alpha1.ForEachDimension{{"region": "${schema.spec.regions}"}},
		}},
	}
}

func assertDifferentHashes(t *testing.T, first, second v1alpha1.ResourceGraphDefinitionSpec) {
	t.Helper()

	hashFirst, err := Spec(first)
	require.NoError(t, err)
	hashSecond, err := Spec(second)
	require.NoError(t, err)
	assert.NotEqual(t, hashFirst, hashSecond)
}

func raw(s string) runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(s)}
}

func withJSONMarshalStub(t *testing.T, stub func(any) ([]byte, error)) {
	t.Helper()

	original := jsonMarshal
	jsonMarshal = stub
	t.Cleanup(func() {
		jsonMarshal = original
	})
}

type badRuntimeObject struct{}

func (badRuntimeObject) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }
func (badRuntimeObject) DeepCopyObject() runtime.Object   { return badRuntimeObject{} }
func (badRuntimeObject) MarshalJSON() ([]byte, error)     { return nil, errors.New("boom") }
