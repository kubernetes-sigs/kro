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

package resourcegroup

import (
	"testing"

	"github.com/aws/symphony/api/v1alpha1"
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
			err := validateRGResourceNames(tt.rg)
			if (err != nil) != tt.expectError {
				t.Errorf("validateRGResourceNames() error = %v, expectError %v", err, tt.expectError)
			}
		})
	}
}

func TestIsSymphonyReservedWord(t *testing.T) {
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
			if got := isSymphonyReservedWord(tt.word); got != tt.expected {
				t.Errorf("isSymphonyReservedWord(%q) = %v, want %v", tt.word, got, tt.expected)
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
