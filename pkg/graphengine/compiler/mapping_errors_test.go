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

package compiler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func TestCompile_RESTMappingErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		err        error
		wantCalls  int
		wantResets int
	}{
		{
			name: "persistent NoMatch terminates after one retry",
			err: &meta.NoKindMatchError{
				GroupKind: schema.GroupKind{Kind: "ConfigMap"}, SearchedVersions: []string{"v1"},
			},
			wantCalls: 2, wantResets: 1,
		},
		{
			name:      "other discovery errors propagate without resetting",
			err:       errors.New("discovery unavailable"),
			wantCalls: 1, wantResets: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sr, _ := testk8s.NewFakeResolver()
			rm := &failingRESTMapper{err: tc.err}
			cmp := NewCompilerWithDependencies(sr, rm)
			prog, err := cmp.Compile(generator.NewGraph("g", generator.WithTemplate("cm", configMap("cfg"))))
			require.ErrorIs(t, err, tc.err)
			assert.ErrorContains(t, err, `build node "cm": rest mapping for /v1, Kind=ConfigMap:`)
			assert.Nil(t, prog)
			assert.Equal(t, tc.wantCalls, rm.calls)
			assert.Equal(t, tc.wantResets, rm.resets)
		})
	}
}

type failingRESTMapper struct {
	meta.RESTMapper
	err           error
	calls, resets int
}

func (m *failingRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	m.calls++
	return nil, m.err
}

func (m *failingRESTMapper) Reset() { m.resets++ }
