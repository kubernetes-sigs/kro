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

package schema

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateSchemaFromCELTypes_NestedArrayFields(t *testing.T) {
	result, err := GenerateSchemaFromCELTypes(map[string]*cel.Type{
		"status.parents[0].name":  cel.StringType,
		"status.parents[0].ready": cel.BoolType,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "object", result.Type)

	status := result.Properties["status"]
	require.Equal(t, "object", status.Type)
	parents := status.Properties["parents"]
	require.Equal(t, "array", parents.Type)
	require.NotNil(t, parents.Items)
	require.NotNil(t, parents.Items.Schema)

	// Sibling fields must survive in the same array items schema.
	item := parents.Items.Schema
	require.Equal(t, "object", item.Type)
	assert.Equal(t, "string", item.Properties["name"].Type, "status.parents[0].name")
	assert.Equal(t, "boolean", item.Properties["ready"].Type, "status.parents[0].ready")
}

func TestGenerateSchemaFromCELTypes_ComplexPath(t *testing.T) {
	result, err := GenerateSchemaFromCELTypes(map[string]*cel.Type{
		"status.parents[0].children[0].metadata.labels[0].key": cel.StringType,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "object", result.Type)

	status := result.Properties["status"]
	require.Equal(t, "object", status.Type)
	parents := status.Properties["parents"]
	require.Equal(t, "array", parents.Type)
	require.NotNil(t, parents.Items)
	require.NotNil(t, parents.Items.Schema)
	require.Equal(t, "object", parents.Items.Schema.Type)

	children := parents.Items.Schema.Properties["children"]
	require.Equal(t, "array", children.Type)
	require.NotNil(t, children.Items)
	require.NotNil(t, children.Items.Schema)
	require.Equal(t, "object", children.Items.Schema.Type)

	metadata := children.Items.Schema.Properties["metadata"]
	require.Equal(t, "object", metadata.Type)
	labels := metadata.Properties["labels"]
	require.Equal(t, "array", labels.Type)
	require.NotNil(t, labels.Items)
	require.NotNil(t, labels.Items.Schema)
	require.Equal(t, "object", labels.Items.Schema.Type)
	assert.Equal(t, "string", labels.Items.Schema.Properties["key"].Type)
}
