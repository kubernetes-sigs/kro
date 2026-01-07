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

package graph

import (
	"strings"
	"testing"

	"github.com/google/cel-go/cel"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
)

func TestValidateRGResourceNames(t *testing.T) {
	tests := []struct {
		name        string
		rgd         *v1alpha1.ResourceGraphDefinition
		expectError bool
	}{
		{
			name: "Valid resource graph definition resource ids",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "validID1"},
						{ID: "validID2"},
					},
				},
			},
			expectError: false,
		},
		{
			name: "Duplicate resource ids",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "duplicateID"},
						{ID: "duplicateID"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Invalid resource ID",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "Invalid_ID"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Reserved word as resource id",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "spec"},
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResourceIDs(tt.rgd)
			if (err != nil) != tt.expectError {
				t.Errorf("validateRGResourceIDs() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestIsKROReservedWord(t *testing.T) {
	tests := []struct {
		word     string
		expected bool
	}{
		{"resourcegraphdefinition", true},
		{"instance", true},
		{"each", true}, // Reserved for per-item readiness in collections
		{"notReserved", false},
		{"RESOURCEGRAPHDEFINITION", false}, // Case-sensitive check
	}

	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			if got := isKROReservedWord(tt.word); got != tt.expected {
				t.Errorf("isKROReservedWord(%q) = %v, want %v", tt.word, got, tt.expected)
			}
		})
	}
}

func TestIsValidResourceName(t *testing.T) {
	tests := []struct {
		name     string
		expected bool
	}{
		{"validName", true},
		{"validName123", true},
		{"123invalidName", false},
		{"invalid_name", false},
		{"InvalidName", false},
		{"valid123Name", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isValidResourceID(tt.name); got != tt.expected {
				t.Errorf("isValidResourceName(%q) = %v, want %v", tt.name, got, tt.expected)
			}
		})
	}
}

func TestValidateKubernetesObjectStructure(t *testing.T) {
	tests := []struct {
		name    string
		obj     map[string]interface{}
		wantErr bool
		errMsg  string
	}{
		{
			name: "Valid Kubernetes object",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   map[string]interface{}{},
			},
			wantErr: false,
		},
		{
			name: "Missing apiVersion",
			obj: map[string]interface{}{
				"kind":     "Pod",
				"metadata": map[string]interface{}{},
			},
			wantErr: true,
			errMsg:  "apiVersion field not found",
		},
		{
			name: "apiVersion not a string",
			obj: map[string]interface{}{
				"apiVersion": 123,
				"kind":       "Pod",
				"metadata":   map[string]interface{}{},
			},
			wantErr: true,
			errMsg:  "apiVersion field is not a string",
		},
		{
			name: "Missing kind",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"metadata":   map[string]interface{}{},
			},
			wantErr: true,
			errMsg:  "kind field not found",
		},
		{
			name: "kind not a string",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       123,
				"metadata":   map[string]interface{}{},
			},
			wantErr: true,
			errMsg:  "kind field is not a string",
		},
		{
			name: "Missing metadata",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
			},
			wantErr: true,
			errMsg:  "metadata field not found",
		},
		{
			name: "metadata not a map",
			obj: map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata":   "not a map",
			},
			wantErr: true,
			errMsg:  "metadata field is not a map",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKubernetesObjectStructure(tt.obj)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateKubernetesObjectStructure() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && err.Error() != tt.errMsg {
				t.Errorf("validateKubernetesObjectStructure() error message = %v, want %v", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestValidateKubernetesVersion(t *testing.T) {
	tests := []struct {
		version    string
		shouldPass bool
	}{
		{"v1", true},
		{"v10", true},
		{"v1beta1", true},
		{"v1beta2", true},
		{"v1alpha1", true},
		{"v1alpha2", true},
		{"v1alpha10", true},
		{"v15alpha1", true},
		{"v2", true},
		{"v", false},
		{"vvvv", false},
		{"v1.1", false},
		{"v1.1.1", false},
		{"v1alpha", false},
		{"valpha1", false},
		{"alpha", false},
		{"1alpha", false},
		{"v1alpha1beta1", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			err := validateKubernetesVersion(tt.version)
			if tt.shouldPass && err != nil {
				t.Errorf("Expected version %q to be valid, but got error: %v", tt.version, err)
			}
			if !tt.shouldPass && err == nil {
				t.Errorf("Expected version %q to be invalid, but it passed validation", tt.version)
			}
		})
	}
}

func TestValidateResourceGraphDefinitionNamingConventions(t *testing.T) {
	tests := []struct {
		name       string
		resourceID string
		kind       string
		wantErr    bool
	}{
		{
			name:       "Valid naming conventions",
			resourceID: "validResourceID",
			kind:       "ValidKindName",
			wantErr:    false,
		},
		{
			name:       "Invalid kind name",
			resourceID: "validResourceID",
			kind:       "invalidKindName",
			wantErr:    true,
		},
		{
			name:       "Invalid resource ID",
			resourceID: "invalid_ResourceID",
			kind:       "ValidKindName",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rgd := &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: tt.resourceID},
					},
					Schema: &v1alpha1.Schema{
						Kind: tt.kind,
					},
				},
			}
			if err := validateResourceGraphDefinitionNamingConventions(rgd); (err != nil) != tt.wantErr {
				t.Errorf("validateResourceGraphDefinitionNamingConventions() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInferListElementType(t *testing.T) {
	tests := []struct {
		name        string
		celType     *cel.Type
		wantElemTyp *cel.Type
		wantErr     bool
	}{
		{
			name:        "list of strings",
			celType:     cel.ListType(cel.StringType),
			wantElemTyp: cel.StringType,
			wantErr:     false,
		},
		{
			name:        "list of ints",
			celType:     cel.ListType(cel.IntType),
			wantElemTyp: cel.IntType,
			wantErr:     false,
		},
		{
			name:        "list of dyn",
			celType:     cel.ListType(cel.DynType),
			wantElemTyp: cel.DynType,
			wantErr:     false,
		},
		{
			name:    "not a list - string type",
			celType: cel.StringType,
			wantErr: true,
		},
		{
			name:    "not a list - int type",
			celType: cel.IntType,
			wantErr: true,
		},
		{
			name:    "not a list - map type",
			celType: cel.MapType(cel.StringType, cel.IntType),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elemType, err := krocel.ListElementType(tt.celType)
			if tt.wantErr {
				if err == nil {
					t.Errorf("ListElementType() expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("ListElementType() unexpected error: %v", err)
				return
			}
			if !tt.wantElemTyp.IsAssignableType(elemType) {
				t.Errorf("ListElementType() = %v, want %v", elemType, tt.wantElemTyp)
			}
		})
	}
}

func TestValidateForEachDimensions(t *testing.T) {
	tests := []struct {
		name        string
		rgd         *v1alpha1.ResourceGraphDefinition
		expectError bool
		errorMsg    string
	}{
		{
			name: "Valid forEach iterator",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "workers",
							ForEach: []v1alpha1.ForEachDimension{
								{"name": "${schema.spec.workers}"},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "Valid multiple forEach iterators",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "deployments",
							ForEach: []v1alpha1.ForEachDimension{
								{"region": "${schema.spec.regions}"},
								{"tier": "${schema.spec.tiers}"},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "Invalid iterator name - not lowerCamelCase",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "workers",
							ForEach: []v1alpha1.ForEachDimension{
								{"Invalid_Name": "${schema.spec.workers}"},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "forEach iterator name",
		},
		{
			name: "Iterator name is reserved keyword (schema)",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "workers",
							ForEach: []v1alpha1.ForEachDimension{
								{"schema": "${schema.spec.workers}"},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "reserved keyword",
		},
		{
			name: "Iterator name 'each' is reserved for per-item readiness",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "pods",
							ForEach: []v1alpha1.ForEachDimension{
								{"each": "${schema.spec.podNames}"},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "reserved keyword",
		},
		{
			name: "Iterator name conflicts with resource ID",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "database"},
						{
							ID: "backups",
							ForEach: []v1alpha1.ForEachDimension{
								{"database": "${schema.spec.databases}"},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "conflicts with resource ID",
		},
		{
			name: "Duplicate iterator names in same resource",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "workers",
							ForEach: []v1alpha1.ForEachDimension{
								{"name": "${schema.spec.workers}"},
								{"name": "${schema.spec.otherWorkers}"},
							},
						},
					},
				},
			},
			expectError: true,
			errorMsg:    "duplicate forEach iterator name",
		},
		{
			name: "Same iterator name in different resources is valid",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{
							ID: "workers",
							ForEach: []v1alpha1.ForEachDimension{
								{"name": "${schema.spec.workers}"},
							},
						},
						{
							ID: "backups",
							ForEach: []v1alpha1.ForEachDimension{
								{"name": "${schema.spec.databases}"},
							},
						},
					},
				},
			},
			expectError: false,
		},
		{
			name: "Resource without forEach is valid",
			rgd: &v1alpha1.ResourceGraphDefinition{
				Spec: v1alpha1.ResourceGraphDefinitionSpec{
					Resources: []*v1alpha1.Resource{
						{ID: "deployment"},
					},
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResourceIDs(tt.rgd)
			if (err != nil) != tt.expectError {
				t.Errorf("validateResourceIDs() error = %v, expectError %v", err, tt.expectError)
			}
			if tt.expectError && err != nil && tt.errorMsg != "" {
				if !strings.Contains(err.Error(), tt.errorMsg) {
					t.Errorf("validateResourceIDs() error = %v, should contain %q", err, tt.errorMsg)
				}
			}
		})
	}
}
