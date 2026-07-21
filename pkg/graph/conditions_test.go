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

package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
)

func TestValidateConditionExpressions(t *testing.T) {
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(
		[]string{"resource", SchemaVarName, library.RuntimeVarName},
	))
	require.NoError(t, err)

	tests := []struct {
		name     string
		exprs    []string
		wantErrs []string
	}{
		{
			name:  "empty list",
			exprs: nil,
		},
		{
			name: "well-formed conditions",
			exprs: []string{
				`runtime.newCondition({type: 'A', status: 'True', reason: 'OK', message: 'all good'})`,
				`runtime.newCondition({type: 'B', status: 'False', reason: '', message: ''})`,
				`runtime.newCondition({type: 'C', status: 'Unknown', reason: '', message: ''})`,
			},
		},
		{
			name: "built-in lookup is not a self-reference, even when the author overrides the type",
			exprs: []string{
				`runtime.newCondition({type: 'ResourcesReady', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'DerivedReady',
					status: runtime.condition(schema, 'ResourcesReady').status,
					reason: '', message: ''})`,
			},
		},
		{
			name: "dynamic status and dynamic type lookup are left to evaluation time",
			exprs: []string{
				`runtime.newCondition({type: 'A', status: schema.spec.someStatus, reason: '', message: ''})`,
				`runtime.condition(schema, schema.spec.someType).status`,
			},
		},
		{
			name: "lookup on a child resource may share a type name with an author condition",
			exprs: []string{
				`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'ChildMirror',
					status: runtime.condition(resource, 'PrimaryReady').status,
					reason: '', message: ''})`,
			},
		},
		{
			name: "repeated type within one entry (ternary branches) is allowed",
			exprs: []string{
				`schema.spec.someStatus == 'up'
					? runtime.newCondition({type: 'AppReady', status: 'True', reason: '', message: ''})
					: runtime.newCondition({type: 'AppReady', status: 'False', reason: '', message: ''})`,
			},
		},
		{
			name:  "parse errors are left to the regular compile step",
			exprs: []string{`this is not (((valid CEL`},
		},
		{
			name: "non-runtime receivers are ignored",
			exprs: []string{
				`schema.spec.condition(schema, 'X')`,
				`schema.newCondition({"extra": 'allowed-here'})`,
			},
		},
		{
			name: "self-reference rejected",
			exprs: []string{
				`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'Ready',
					status: runtime.condition(schema, 'PrimaryReady').status,
					reason: '', message: ''})`,
			},
			wantErrs: []string{"custom conditions cannot reference each other", `"PrimaryReady"`},
		},
		{
			name: "invalid literal status rejected",
			exprs: []string{
				`runtime.newCondition({type: 'X', status: 'YES', reason: 'R', message: 'M'})`,
			},
			wantErrs: []string{"status must be one of"},
		},
		{
			name: "invalid literal status inside a map() comprehension rejected",
			exprs: []string{
				`schema.spec.servers.map(s,
					runtime.newCondition({type: s.name, status: 'BAD', reason: '', message: ''}))`,
			},
			wantErrs: []string{"status must be one of"},
		},
		{
			name: "empty literal type rejected",
			exprs: []string{
				`runtime.newCondition({type: '', status: 'True', reason: '', message: ''})`,
			},
			wantErrs: []string{"type must not be empty"},
		},
		{
			name: "duplicate literal type across entries rejected",
			exprs: []string{
				`runtime.newCondition({type: 'AppReady', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'AppReady', status: 'False', reason: '', message: ''})`,
			},
			wantErrs: []string{`condition type "AppReady" is declared by more than one conditions entry`},
		},
		{
			name: "unknown literal schema lookup rejected (typo guard)",
			exprs: []string{
				`runtime.newCondition({type: 'Derived',
					status: runtime.condition(schema, 'ResourcesRaedy').status, reason: '', message: ''})`,
			},
			wantErrs: []string{"unknown condition type", `"ResourcesRaedy"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConditionExpressions(env, tt.exprs)
			if len(tt.wantErrs) == 0 {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.wantErrs {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}
