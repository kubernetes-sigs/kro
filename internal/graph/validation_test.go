// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"testing"

	"github.com/awslabs/kro/api/v1alpha1"
)

func TestValidateRGResourceNames(t *testing.T) {
	tests := []struct {
		name        string
		rg          *v1alpha1.ResourceGroup
		expectError bool
	}{
		{
			name: "Valid resource group resource names",
			rg: &v1alpha1.ResourceGroup{
				Spec: v1alpha1.ResourceGroupSpec{
					Resources: []*v1alpha1.Resource{
						{Name: "validName1"},
						{Name: "validName2"},
					},
				},
			},
			expectError: false,
		},
		{
			name: "Duplicate resource names",
			rg: &v1alpha1.ResourceGroup{
				Spec: v1alpha1.ResourceGroupSpec{
					Resources: []*v1alpha1.Resource{
						{Name: "duplicateName"},
						{Name: "duplicateName"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Invalid resource name",
			rg: &v1alpha1.ResourceGroup{
				Spec: v1alpha1.ResourceGroupSpec{
					Resources: []*v1alpha1.Resource{
						{Name: "Invalid_Name"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "Reserved word as resource name",
			rg: &v1alpha1.ResourceGroup{
				Spec: v1alpha1.ResourceGroupSpec{
					Resources: []*v1alpha1.Resource{
						{Name: "spec"},
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResourceNames(tt.rg)
			if (err != nil) != tt.expectError {
				t.Errorf("validateRGResourceNames() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestIsKROReservedWord(t *testing.T) {
	tests := []struct {
		word     string
		expected bool
	}{
		{"resourcegroup", true},
		{"instance", true},
		{"notReserved", false},
		{"RESOURCEGROUP", false}, // Case-sensitive check
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
			if got := isValidResourceName(tt.name); got != tt.expected {
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
