// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package cel

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
)

func TestWithResourceIDs(t *testing.T) {
	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{
			name: "empty ids",
			ids:  []string{},
			want: []string(nil),
		},
		{
			name: "single id",
			ids:  []string{"resource1"},
			want: []string{"resource1"},
		},
		{
			name: "multiple ids",
			ids:  []string{"resource1", "resource2", "resource3"},
			want: []string{"resource1", "resource2", "resource3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &envOptions{}
			WithResourceIDs(tt.ids)(opts)
			assert.Equal(t, tt.want, opts.resourceIDs)
		})
	}
}

func TestWithCustomDeclarations(t *testing.T) {
	tests := []struct {
		name         string
		declarations []cel.EnvOption
		wantLen      int
	}{
		{
			name:         "empty declarations",
			declarations: []cel.EnvOption{},
			wantLen:      0,
		},
		{
			name:         "single declaration",
			declarations: []cel.EnvOption{cel.Variable("test", cel.StringType)},
			wantLen:      1,
		},
		{
			name: "multiple declarations",
			declarations: []cel.EnvOption{
				cel.Variable("test1", cel.AnyType),
				cel.Variable("test2", cel.StringType),
			},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := &envOptions{}
			WithCustomDeclarations(tt.declarations)(opts)
			assert.Len(t, opts.customDeclarations, tt.wantLen)
		})
	}
}

func TestDefaultEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		options []EnvOption
		wantErr bool
	}{
		{
			name:    "no options",
			options: nil,
			wantErr: false,
		},
		{
			name: "with resource IDs",
			options: []EnvOption{
				WithResourceIDs([]string{"resource1", "resource2"}),
			},
			wantErr: false,
		},
		{
			name: "with custom declarations",
			options: []EnvOption{
				WithCustomDeclarations([]cel.EnvOption{
					cel.Variable("custom", cel.StringType),
				}),
			},
			wantErr: false,
		},
		{
			name: "with both resource IDs and custom declarations",
			options: []EnvOption{
				WithResourceIDs([]string{"resource1"}),
				WithCustomDeclarations([]cel.EnvOption{
					cel.Variable("custom", cel.StringType),
				}),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := DefaultEnvironment(tt.options...)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.NotNil(t, env)
		})
	}
}
