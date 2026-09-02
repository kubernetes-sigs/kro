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

package rgdadapter

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apimachineryruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
)

// stubCompiler records what BuildRuntimeForInstance handed the compiler so a
// test can assert on the Graph shape and the option count without needing a
// real CEL environment.
type stubCompiler struct {
	prog     *compiler.Program
	err      error
	gotGraph *v1alpha1.Graph
	gotOpts  int
	calls    int
}

func (s *stubCompiler) CompileWithOptions(
	g *v1alpha1.Graph, opts ...compiler.CompileOption,
) (*compiler.Program, error) {
	s.calls++
	s.gotGraph = g
	s.gotOpts = len(opts)
	if s.err != nil {
		return nil, s.err
	}
	return s.prog, nil
}

func emptyProgram() *compiler.Program {
	return &compiler.Program{
		Nodes:            map[string]*compiler.Node{},
		TopologicalOrder: []string{},
	}
}

func testInstance(name, namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1", "kind": "WebApp",
		"metadata": map[string]any{"name": name, "namespace": namespace},
		"spec":     map[string]any{"replicas": int64(1)},
	}}
}

func testRGD(schema *v1alpha1.Schema) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: schema,
			Resources: []*v1alpha1.Resource{{
				ID: "cm",
				Template: apimachineryruntime.RawExtension{
					Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cm"}}`),
				},
			}},
		},
	}
}

// BuildRuntimeForInstance is called on every instance reconcile, so its guards
// are the difference between a descriptive condition and a nil-pointer panic
// in the instance controller.
func TestBuildRuntimeForInstance_Guards(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rgd      *v1alpha1.ResourceGraphDefinition
		instance *unstructured.Unstructured
		compiler Compiler
		wantErr  string
	}{
		{
			name:     "a nil RGD is rejected",
			instance: testInstance("demo", "default"),
			compiler: &stubCompiler{prog: emptyProgram()},
			wantErr:  "rgd is required",
		},
		{
			name:     "a nil instance is rejected",
			rgd:      testRGD(nil),
			compiler: &stubCompiler{prog: emptyProgram()},
			wantErr:  "instance is required",
		},
		{
			name:     "a nil compiler is rejected",
			rgd:      testRGD(nil),
			instance: testInstance("demo", "default"),
			wantErr:  "compiler is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, g, err := BuildRuntimeForInstance(tt.rgd, tt.instance, tt.compiler)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Nil(t, rt)
			assert.Nil(t, g)
		})
	}
}

// Errors from each stage are wrapped with the stage that produced them, so an
// operator reading the instance's GraphResolved condition can tell a bad RGD
// shape from a compile failure.
func TestBuildRuntimeForInstance_WrapsStageErrors(t *testing.T) {
	t.Parallel()

	t.Run("a translation failure is reported as translate", func(t *testing.T) {
		t.Parallel()
		// Neither template nor externalRef: rejected during translation.
		rgd := &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
			Spec: v1alpha1.ResourceGraphDefinitionSpec{
				Resources: []*v1alpha1.Resource{{ID: "broken"}},
			},
		}
		stub := &stubCompiler{prog: emptyProgram()}

		_, _, err := BuildRuntimeForInstance(rgd, testInstance("demo", "default"), stub)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "translate")
		assert.True(t, errors.Is(err, ErrUnsupported),
			"the underlying ErrUnsupported must survive wrapping")
		assert.Zero(t, stub.calls, "translation must fail before the compiler is reached")
	})

	t.Run("a compile failure is reported as compile", func(t *testing.T) {
		t.Parallel()
		stub := &stubCompiler{err: errors.New("type mismatch at cm.metadata.name")}

		_, _, err := BuildRuntimeForInstance(testRGD(nil), testInstance("demo", "default"), stub)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "compile")
		assert.Contains(t, err.Error(), "type mismatch",
			"the compiler's own message must be preserved")
	})
}

// The Graph handed to the compiler has to carry the instance's identity and the
// schema node, or ${schema.spec.*} will not resolve and namespaced resources
// will default to the wrong namespace.
func TestBuildRuntimeForInstance_GraphShape(t *testing.T) {
	t.Parallel()

	stub := &stubCompiler{prog: emptyProgram()}
	rt, g, err := BuildRuntimeForInstance(testRGD(nil), testInstance("demo", "team-a"), stub)
	require.NoError(t, err)
	require.NotNil(t, rt)
	require.NotNil(t, g)

	require.NotNil(t, stub.gotGraph)
	require.NotEmpty(t, stub.gotGraph.Spec.Nodes)

	assert.Equal(t, SchemaNodeID, stub.gotGraph.Spec.Nodes[0].ID,
		"the schema node must be prepended so it precedes every resource node")
	assert.Equal(t, "cm", stub.gotGraph.Spec.Nodes[1].ID,
		"resource nodes must follow the schema node in RGD order")

	assert.Equal(t, "demo", stub.gotGraph.Name,
		"the Graph is named for the instance, not the RGD")
	assert.Equal(t, "team-a", stub.gotGraph.Namespace,
		"the executor reads the Graph namespace to default namespaced resources")
}

// The schema node is typed from the RGD's declared SimpleSchema rather than
// inferred from the current instance's values, so a fresh instance missing
// optional fields still type-checks identically. That override is what keeps
// the compiled Program instance-independent.
func TestBuildRuntimeForInstance_SchemaOverride(t *testing.T) {
	t.Parallel()

	t.Run("a declared schema produces a node schema override", func(t *testing.T) {
		t.Parallel()
		rgd := testRGD(&v1alpha1.Schema{
			Kind:       "WebApp",
			APIVersion: "v1alpha1",
			Group:      "kro.run",
			Spec: apimachineryruntime.RawExtension{
				Raw: []byte(`{"replicas":"integer"}`),
			},
		})
		stub := &stubCompiler{prog: emptyProgram()}

		_, _, err := BuildRuntimeForInstance(rgd, testInstance("demo", "default"), stub)
		require.NoError(t, err)
		assert.Equal(t, 2, stub.gotOpts,
			"a declared SimpleSchema must be passed through as a node schema override alongside WithLiteralNode")
	})

	t.Run("no declared schema passes no options", func(t *testing.T) {
		t.Parallel()
		stub := &stubCompiler{prog: emptyProgram()}

		_, _, err := BuildRuntimeForInstance(testRGD(nil), testInstance("demo", "default"), stub)
		require.NoError(t, err)
		assert.Equal(t, 1, stub.gotOpts,
			"without a declared schema the schema node stays untyped rather than "+
				"being inferred from this instance's values (WithLiteralNode only)")
	})
}
