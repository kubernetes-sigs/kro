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

package library

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/require"
)

func TestSimpleSchemaToOpenAPI(t *testing.T) {
	env, err := cel.NewEnv(SimpleSchema())
	require.NoError(t, err)

	out := evalCEL(t, env, `simpleschema.toOpenAPI({'name': 'string | required=true', 'kind': 'string'}, {})`)
	require.Equal(t, map[string]any{
		"type":     "object",
		"required": []any{"name"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"},
			"kind": map[string]any{"type": "string"},
		},
	}, out.Value())

	port := evalCEL(t, env, `simpleschema.toOpenAPI({'listeners': '[]Listener'},
		{'Listener': {'port': 'integer | default=80'}}).properties.listeners.items.properties.port`)
	require.Equal(t, map[string]any{"type": "integer", "default": int64(80)}, port.Value())
}

func TestSimpleSchemaToOpenAPIErrors(t *testing.T) {
	env, err := cel.NewEnv(SimpleSchema(), cel.OptionalTypes())
	require.NoError(t, err)
	for _, tc := range []struct{ expr, want string }{
		{`simpleschema.toOpenAPI([], {})`, "fields: expected a map"},
		{`simpleschema.toOpenAPI({}, null)`, "types: expected a map"},
		{`simpleschema.toOpenAPI(optional.of({}), {})`, "fields: expected a map"},
		{`simpleschema.toOpenAPI({1: 'string'}, {})`, "map key must be string"},
		{`simpleschema.toOpenAPI({'port': 'Missing'}, {})`, "field port: unknown type: Missing"},
		{`simpleschema.toOpenAPI({'limit': 'float | maximum=NaN'}, {})`, "unsupported value: NaN"},
	} {
		t.Run(tc.expr, func(t *testing.T) { require.Contains(t, evalErr(t, env, tc.expr), tc.want) })
	}
}
