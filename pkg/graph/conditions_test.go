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
			name: "cross-reference between entries is allowed",
			exprs: []string{
				`runtime.newCondition({type: 'PrimaryReady', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'Ready',
					status: runtime.condition(schema, 'PrimaryReady').status,
					reason: '', message: ''})`,
			},
		},
		{
			name: "an entry reading a type it declares itself is rejected",
			exprs: []string{
				`runtime.newCondition({type: 'Loop',
					status: runtime.condition(schema, 'Loop').status,
					reason: '', message: ''})`,
			},
			wantErrs: []string{"cannot depend on a condition type it may itself declare", `"Loop"`},
		},
		{
			name: "reference cycle between two entries is rejected",
			exprs: []string{
				`runtime.newCondition({type: 'A',
					status: runtime.condition(schema, 'B').status, reason: '', message: ''})`,
				`runtime.newCondition({type: 'B',
					status: runtime.condition(schema, 'A').status, reason: '', message: ''})`,
			},
			wantErrs: []string{"reference cycle", "entry 0 (A)", "entry 1 (B)"},
		},
		{
			name: "a read matched by a dynamic type pattern is allowed",
			exprs: []string{
				`schema.spec.servers.map(s,
					runtime.newCondition({type: 'Shard-' + s.name, status: 'True', reason: '', message: ''}))`,
				`runtime.newCondition({type: 'Summary',
					status: runtime.condition(schema, 'Shard-web').status, reason: '', message: ''})`,
			},
		},
		{
			name: "type expression with too many alternatives is rejected",
			exprs: []string{
				`runtime.newCondition({type:
					(schema.spec.a ? 'a0' : 'b0') + (schema.spec.b ? 'a1' : 'b1') +
					(schema.spec.c ? 'a2' : 'b2') + (schema.spec.d ? 'a3' : 'b3') +
					(schema.spec.e ? 'a4' : 'b4') + (schema.spec.f ? 'a5' : 'b5'),
					status: 'True', reason: '', message: ''})`,
			},
			wantErrs: []string{"more than 32 possible values", "split it across separate conditions entries"},
		},
		{
			name: "duplicate type across ternary branches of different entries is rejected",
			exprs: []string{
				`schema.spec.p
					? runtime.newCondition({type: 'Primary', status: 'True', reason: '', message: ''})
					: runtime.newCondition({type: 'Replica', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'Primary', status: 'False', reason: '', message: ''})`,
			},
			wantErrs: []string{`condition type "Primary" is declared by more than one conditions entry`},
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
		{
			name: "literal type held to the Kubernetes condition-type format",
			exprs: []string{
				`runtime.newCondition({type: 'Ready Set', status: 'True', reason: '', message: ''})`,
			},
			wantErrs: []string{"is not a valid condition type", `"Ready Set"`},
		},
		{
			name: "invalid type in a ternary branch is rejected",
			exprs: []string{
				`runtime.newCondition({type: schema.spec.ha ? 'Primary' : 'Bad|Name',
					status: 'True', reason: '', message: ''})`,
			},
			wantErrs: []string{"is not a valid condition type", `"Bad|Name"`},
		},
		{
			name: "dotted and path-prefixed type names are accepted",
			exprs: []string{
				`runtime.newCondition({type: 'Ready.v2', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'app.kubernetes.io/Ready', status: 'True', reason: '', message: ''})`,
				`runtime.newCondition({type: 'acme.io/db-ready', status: 'True', reason: '', message: ''})`,
			},
		},
		{
			name: "computed type names are not format-checked",
			exprs: []string{
				`resource.items.map(i, runtime.newCondition({type: 'Shard-' + i.name,
					status: 'True', reason: '', message: ''}))`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateAndOrderConditions(env, tt.exprs)
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

func TestDescribeCycleEntriesAlwaysHavePatterns(t *testing.T) {
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(
		[]string{"shards", SchemaVarName, library.RuntimeVarName},
	))
	require.NoError(t, err)

	link := func(typ, dep string) string {
		return `runtime.newCondition({type: ` + typ +
			`, status: runtime.condition(schema, '` + dep + `').status, reason: '', message: ''})`
	}

	tests := []struct {
		name  string
		exprs []string
	}{
		{
			name:  "two entries, exact types",
			exprs: []string{link(`'A'`, "B"), link(`'B'`, "A")},
		},
		{
			name:  "three entries",
			exprs: []string{link(`'A'`, "C"), link(`'B'`, "A"), link(`'C'`, "B")},
		},
		{
			name:  "four entries",
			exprs: []string{link(`'A'`, "D"), link(`'B'`, "A"), link(`'C'`, "B"), link(`'D'`, "C")},
		},
		{
			name: "fan-out entry in the cycle",
			exprs: []string{
				`shards.map(s, ` + link(`'Shard-' + s.metadata.name`, "Rollup") + `)`,
				link(`'Rollup'`, "Shard-web"),
			},
		},
		{
			name: "ternary entry in the cycle",
			exprs: []string{
				`schema.spec.ha ? ` + link(`'Primary'`, "Rollup") + ` : ` + link(`'Standalone'`, "Rollup"),
				link(`'Rollup'`, "Primary"),
			},
		},
		{
			name: "entries outside the cycle do not appear in it",
			exprs: []string{
				link(`'A'`, "B"),
				link(`'B'`, "A"),
				`runtime.newCondition({type: 'Bystander', status: 'True', reason: '', message: ''})`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for range 50 {
				_, err := validateAndOrderConditions(env, tt.exprs)
				require.Error(t, err)
				require.Contains(t, err.Error(), "reference cycle")
				assert.NotContains(t, err.Error(), "()",
					"a cycle segment rendered with no pattern name: %s", err)
			}
		})
	}
}
