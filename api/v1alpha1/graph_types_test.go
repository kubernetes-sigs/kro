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

package v1alpha1

import (
	"regexp"
	"testing"
)

// serviceAccountNamePattern must stay in sync with the
// +kubebuilder:validation:Pattern marker on GraphSpec.ServiceAccountName in
// graph_types.go. The pattern is only enforced by the apiserver via the
// generated CRD OpenAPI schema, so this test guards the regex contract that
// the marker encodes: a Kubernetes ServiceAccount name is an RFC 1123
// subdomain (dots allowed), not a bare RFC 1123 label.
const serviceAccountNamePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`

func TestGraphSpec_ServiceAccountNamePattern(t *testing.T) {
	re := regexp.MustCompile(serviceAccountNamePattern)

	tests := []struct {
		name    string
		value   string
		allowed bool
	}{
		{name: "simple label", value: "my-sa", allowed: true},
		{name: "single character", value: "a", allowed: true},
		{name: "numeric", value: "123", allowed: true},
		// Regression: RFC 1123 subdomains allow dots. These were wrongly
		// rejected by the old bare-label pattern.
		{name: "dotted subdomain", value: "my.service.account", allowed: true},
		{name: "dotted with hyphens", value: "my-app.team-a.example", allowed: true},
		{name: "leading uppercase rejected", value: "Bad_Name", allowed: false},
		{name: "underscore rejected", value: "bad_name", allowed: false},
		{name: "leading dot rejected", value: ".invalid", allowed: false},
		{name: "trailing dot rejected", value: "invalid.", allowed: false},
		{name: "double dot rejected", value: "a..b", allowed: false},
		{name: "leading hyphen rejected", value: "-invalid", allowed: false},
		{name: "empty rejected", value: "", allowed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := re.MatchString(tt.value)
			if got != tt.allowed {
				t.Errorf("pattern.MatchString(%q) = %v, want %v", tt.value, got, tt.allowed)
			}
		})
	}
}
