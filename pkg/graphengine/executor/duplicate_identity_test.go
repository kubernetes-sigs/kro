// Copyright 2026 The Kube Resource Orchestrator Authors.
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

package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/watchrouter"
)

// TestApply_RejectsDuplicateIdentityBeforeWrite pins finding 207/1627: two
// DISTINCT template nodes rendering the same object identity (GVK+ns+name) must
// be rejected with a hard ErrDuplicateIdentity BEFORE the second node's write,
// so the second node cannot clobber the first's object. The pre-write claim
// guard (prepareItem) fires as soon as the second node is prepared.
func TestApply_RejectsDuplicateIdentityBeforeWrite(t *testing.T) {
	t.Parallel()

	// Two template nodes, cm1 and cm2, both render ConfigMap default/shared.
	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("cm1", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "shared"},
			"data":     map[string]any{"from": "cm1"},
		}),
		generator.WithTemplate("cm2", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "shared"},
			"data":     map[string]any{"from": "cm2"},
		}),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateIdentity),
		"a cross-node duplicate identity must surface ErrDuplicateIdentity, got %v", err)
	assert.False(t, errors.Is(err, ErrNotReady),
		"a duplicate identity is a permanent graph error, not a soft not-ready")
}

// TestApply_RejectsDuplicateIdentityWithinOneCollection pins finding 3901183693:
// two rows of a SINGLE forEach collection node that resolve to the same final
// object identity must be rejected with a hard ErrDuplicateIdentity, not applied
// concurrently with a nondeterministic last-writer win reported as success.
// The forEach iterator appears in metadata.namespace (satisfying the compile-
// time uniqueness guard), but the values "" and "default" both default to the
// Graph namespace "default", so both rows resolve to ConfigMap default/shared.
func TestApply_RejectsDuplicateIdentityWithinOneCollection(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"namespaces": []any{"", "default"}}),
		generator.WithTemplate("cm", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "shared", "namespace": "${ns}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("ns", "${src.namespaces}")),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	_, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDuplicateIdentity),
		"two collection rows colliding on one identity must surface ErrDuplicateIdentity, got %v", err)
}

// TestApply_AllowsDistinctIdentities confirms the guard does not false-positive
// on two nodes that render DIFFERENT identities.
func TestApply_AllowsDistinctIdentities(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithTemplate("cm1", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "one"},
			"data":     map[string]any{"k": "v"},
		}),
		generator.WithTemplate("cm2", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "two"},
			"data":     map[string]any{"k": "v"},
		}),
	)

	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).Build()
	res, err := NewSimple(cl).Apply(context.Background(), compileAndBuild(t, g), watchrouter.NoopWatcher{})

	require.NoError(t, err, "distinct identities must not trip the duplicate guard")
	assert.Len(t, res.Applied, 2, "both distinct objects apply")
}
