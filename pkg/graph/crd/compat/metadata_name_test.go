// Copyright 2026 The Kubernetes Authors
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

package compat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestCompareInstanceMetadataName(t *testing.T) {
	length := func(value int64) *int64 { return &value }
	schema := func(name *v1.JSONSchemaProps) *v1.JSONSchemaProps {
		metadata := v1.JSONSchemaProps{Type: "object"}
		if name != nil {
			metadata.Properties = map[string]v1.JSONSchemaProps{"name": *name}
		}
		return &v1.JSONSchemaProps{
			Type: "object",
			Properties: map[string]v1.JSONSchemaProps{
				"metadata": metadata,
			},
		}
	}

	tests := []struct {
		name       string
		oldName    *v1.JSONSchemaProps
		newName    *v1.JSONSchemaProps
		changeType ChangeType
		breaking   bool
	}{
		{name: "added constraint", newName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(30)}, changeType: MaxLengthAdded, breaking: true},
		{name: "tightened constraint", oldName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(30)}, newName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(20)}, changeType: MaxLengthDecreased, breaking: true},
		{name: "loosened constraint", oldName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(20)}, newName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(30)}, changeType: MaxLengthIncreased},
		{name: "removed constraint", oldName: &v1.JSONSchemaProps{Type: "string", MaxLength: length(30)}, changeType: MaxLengthRemoved},
		{name: "added CEL", newName: &v1.JSONSchemaProps{Type: "string", XValidations: v1.ValidationRules{{Rule: "self != 'reserved'"}}}, changeType: ValidationRulesChanged, breaking: true},
		{name: "changed CEL", oldName: &v1.JSONSchemaProps{Type: "string", XValidations: v1.ValidationRules{{Rule: "self != 'reserved'"}}}, newName: &v1.JSONSchemaProps{Type: "string", XValidations: v1.ValidationRules{{Rule: "self != 'blocked'"}}}, changeType: ValidationRulesChanged, breaking: true},
		{name: "removed CEL", oldName: &v1.JSONSchemaProps{Type: "string", XValidations: v1.ValidationRules{{Rule: "self != 'reserved'"}}}, changeType: ValidationRulesChanged},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, conservative := range []bool{false, true} {
				report := Compare(schema(tt.oldName), schema(tt.newName), WithConservativeComparison(conservative))
				changes := report.NonBreakingChanges
				if tt.breaking {
					changes = report.BreakingChanges
				}
				require.Len(t, changes, 1, "conservative=%t: %#v", conservative, report)
				assert.Equal(t, tt.changeType, changes[0].ChangeType)
				assert.Equal(t, instanceNameSchemaPath, changes[0].Path[:len(instanceNameSchemaPath)])
				assert.Equal(t, tt.breaking, report.HasBreakingChanges())
			}
		})
	}
}
