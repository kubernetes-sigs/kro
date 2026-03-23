// Copyright 2025 The Kubernetes Authors.
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

package schema

import (
	"testing"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSchemaFromEvals_NestedFields(t *testing.T) {
	evals := map[string][]ref.Val{
		"status.parent.child": {types.String("value")},
	}

	result, err := GenerateSchemaFromEvals(evals)
	require.NoError(t, err)
	require.NotNil(t, result)

	status, ok := result.Properties["status"]
	require.True(t, ok)

	parent, ok := status.Properties["parent"]
	require.True(t, ok)

	child, ok := parent.Properties["child"]
	require.True(t, ok)

	assert.Equal(t, "string", child.Type)
}

func TestGenerateSchemaFromEvals_NestedArrayFields(t *testing.T) {
	evals := map[string][]ref.Val{
		"status.parents[0].child": {types.String("value")},
	}

	result, err := GenerateSchemaFromEvals(evals)
	require.NoError(t, err)
	require.NotNil(t, result)

	status, ok := result.Properties["status"]
	require.True(t, ok)

	parents, ok := status.Properties["parents"]
	require.True(t, ok)
	require.Equal(t, "array", parents.Type)
	require.NotNil(t, parents.Items)
	require.NotNil(t, parents.Items.Schema)

	child, ok := parents.Items.Schema.Properties["child"]
	require.True(t, ok)
	assert.Equal(t, "string", child.Type)
}

func TestGenerateSchemaFromEvals_ComplexPath(t *testing.T) {
	evals := map[string][]ref.Val{
		"status.parents[0].children[0].metadata.labels[0].key": {types.String("value")},
	}

	result, err := GenerateSchemaFromEvals(evals)
	require.NoError(t, err)
	require.NotNil(t, result)

	status, ok := result.Properties["status"]
	require.True(t, ok)
	assert.Equal(t, "object", status.Type)

	parents, ok := status.Properties["parents"]
	require.True(t, ok)
	require.Equal(t, "array", parents.Type)
	require.NotNil(t, parents.Items)
	require.NotNil(t, parents.Items.Schema)

	children, ok := parents.Items.Schema.Properties["children"]
	require.True(t, ok)
	require.Equal(t, "array", children.Type)
	require.NotNil(t, children.Items)
	require.NotNil(t, children.Items.Schema)

	metadata, ok := children.Items.Schema.Properties["metadata"]
	require.True(t, ok)
	assert.Equal(t, "object", metadata.Type)

	labels, ok := metadata.Properties["labels"]
	require.True(t, ok)
	require.Equal(t, "array", labels.Type)
	require.NotNil(t, labels.Items)
	require.NotNil(t, labels.Items.Schema)

	key, ok := labels.Items.Schema.Properties["key"]
	require.True(t, ok)
	assert.Equal(t, "string", key.Type)
}
