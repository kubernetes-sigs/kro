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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/cel/openapi"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	celunstructured "github.com/kubernetes-sigs/kro/pkg/cel/unstructured"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
)

// Compiler is the interface subset of compiler.Compiler that
// BuildRuntimeForInstance needs. Production callers pass *compiler.Compiler
// directly; tests may pass a narrower stub.
type Compiler interface {
	CompileWithOptions(g *v1alpha1.Graph, opts ...compiler.CompileOption) (*compiler.Program, error)
}

// ProgramCache is the subset of *registry.Registry that
// BuildRuntimeForInstanceCached needs: a compile-through cache keyed by
// (owner, spec-hash). *registry.Registry satisfies it. A nil cache disables
// caching (the caller falls back to the inline compile path).
type ProgramCache interface {
	Compile(key types.NamespacedName, g *v1alpha1.Graph, compile registry.CompileFunc) (*compiler.Program, bool, error)
}

// BuildRuntimeForInstance is the single adapter entrypoint the instance
// controller calls for every reconcile cycle:
//
//  1. Translate the RGD's resources into a Graph (ResourceGraphDefinitionToGraph).
//  2. Prepend an InstanceSchemaNode so ${schema.spec.*} references resolve.
//  3. Set Graph metadata (name + namespace) from the instance so the executor
//     can default namespaced resources to the correct namespace.
//  4. Compile the Graph via c.CompileWithOptions.
//  5. Construct and return a runtime.Runtime ready for executor.Simple.Apply.
//
// This inline path bakes the instance's data into the compiled `schema` node,
// so the resulting Program is instance-specific and recompiled every cycle.
// Callers that hold a ProgramCache should prefer BuildRuntimeForInstanceCached,
// which shares one compiled Program across every instance of a revision.
//
// On success the caller owns the returned Runtime and Graph; they are
// discarded after each reconcile cycle (Runtime is single-use by design).
func BuildRuntimeForInstance(
	rgd *v1alpha1.ResourceGraphDefinition,
	instance *unstructured.Unstructured,
	c Compiler,
	opts ...runtime.Option,
) (*runtime.Runtime, *v1alpha1.Graph, error) {
	if err := validateBuildInputs(rgd, instance, c); err != nil {
		return nil, nil, err
	}

	// Step 1: translate RGD resources → Graph nodes.
	g, err := ResourceGraphDefinitionToGraph(rgd)
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: translate: %w", err)
	}

	// Step 2: prepend the instance as a `schema` def node (data baked in).
	schemaNode, err := InstanceSchemaNode(instance)
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: schema node: %w", err)
	}
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	// Step 3: stamp the Graph's metadata so the executor can namespace-default
	// namespaced resources correctly (executor reads rt.Graph().GetNamespace()).
	stampGraphMeta(g, instance)

	// Step 4: compile.
	compileOpts, err := schemaCompileOpts(rgd, g)
	if err != nil {
		return nil, nil, err
	}
	prog, err := c.CompileWithOptions(g, compileOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: compile: %w", err)
	}

	// Step 5: construct the runtime.
	var rtOpts []runtime.Option
	if rgd.Spec.Schema != nil {
		if schemaVarSchema, err := graph.InstanceSchemaForCEL(rgd); err == nil && schemaVarSchema != nil {
			if schemaData, err := instanceSchemaValue(instance); err == nil {
				rtOpts = append(rtOpts, runtime.WithSeedScope(map[string]any{
					SchemaNodeID: celunstructured.UnstructuredToVal(schemaData, &openapi.Schema{Schema: schemaVarSchema}),
				}))
			}
		}
	}
	rtOpts = append(rtOpts, opts...)
	rt := runtime.New(prog, g, rtOpts...)
	return rt, g, nil
}

// BuildRuntimeForInstanceCached is BuildRuntimeForInstance with the compiled
// Program shared across every instance of a revision via cache, eliminating
// the per-reconcile compile cost (~430µs / 8k+ allocs even for a 1-item graph).
//
// It works because the `schema` def node is marked literal
// (compiler.WithLiteralNode): the instance's data is never parsed as CEL, so
// the compiled Program depends only on the RGD spec and the schema TYPE
// (declared SimpleSchema), not on any instance's concrete values. This function
// therefore compiles the graph with an EMPTY, schema-typed `schema` node — the
// spec hash and resulting Program are identical for every instance of a
// revision — and injects each instance's actual data at runtime via
// runtime.WithNodeObjectOverride. Because the per-instance data lives on the
// per-Runtime Node wrapper (deep-copied on render) and never in the shared
// Program, concurrent reconciles for different instances cannot leak data into
// one another.
//
// The cache is keyed by (owner + schema fingerprint, spec-hash); a spec or
// schema change recompiles and replaces the entry. When cache is nil, or the
// RGD declares no schema (so the empty node cannot be typed), this falls back
// to the inline BuildRuntimeForInstance path.
func BuildRuntimeForInstanceCached(
	rgd *v1alpha1.ResourceGraphDefinition,
	instance *unstructured.Unstructured,
	c Compiler,
	cache ProgramCache,
	opts ...runtime.Option,
) (*runtime.Runtime, *v1alpha1.Graph, error) {
	if err := validateBuildInputs(rgd, instance, c); err != nil {
		return nil, nil, err
	}
	// The cached fast path needs a declared schema to TYPE the empty `schema`
	// node. Without a cache or a schema, fall back to the inline path (which
	// infers the type from the baked-in instance value).
	if cache == nil || rgd.Spec.Schema == nil {
		return BuildRuntimeForInstance(rgd, instance, c, opts...)
	}

	// Per-instance schema DATA — injected at runtime, never compiled in.
	schemaData, err := instanceSchemaValue(instance)
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: schema value: %w", err)
	}

	// Translate + prepend the EMPTY, schema-typed `schema` node. The resulting
	// GraphSpec is byte-identical for every instance of this revision, so the
	// cache hashes to the same key.
	g, err := ResourceGraphDefinitionToGraph(rgd)
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: translate: %w", err)
	}
	g.Spec.Nodes = append([]v1alpha1.Node{emptySchemaNode()}, g.Spec.Nodes...)

	// ObjectMeta carries the per-instance name/namespace for the executor's
	// namespace-defaulting. It is NOT part of GraphSpec, so the cache hash
	// (registry.HashSpec, spec-only) is unaffected and instances still hit.
	stampGraphMeta(g, instance)

	// Compile through the cache. The cache key incorporates a deterministic
	// fingerprint of rgd.Spec.Schema because the graph g prepends an empty
	// schema node; the declared SimpleSchema types arrive via the
	// WithNodeSchemaOverride compile option and are therefore not captured
	// in g.Spec (which registry.HashSpec hashes). Without the schema in the
	// key, revisions that change only spec.schema would hash identically and
	// falsely hit the cache with a stale compiled Program. compileOpts are
	// computed lazily inside the closure so a cache hit skips both the compile
	// and the option build.
	key := types.NamespacedName{
		Namespace: rgd.Namespace,
		Name:      rgd.Name + "|" + schemaFingerprint(rgd.Spec.Schema),
	}
	prog, _, err := cache.Compile(key, g, func(gg *v1alpha1.Graph) (*compiler.Program, error) {
		compileOpts, optErr := schemaCompileOpts(rgd, gg)
		if optErr != nil {
			return nil, optErr
		}
		return c.CompileWithOptions(gg, compileOpts...)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("rgdadapter: compile: %w", err)
	}

	// Inject this instance's schema data as the `schema` node's runtime value.
	var rtOpts []runtime.Option
	if schemaVarSchema, err := graph.InstanceSchemaForCEL(rgd); err == nil && schemaVarSchema != nil {
		rtOpts = append(rtOpts, runtime.WithSeedScope(map[string]any{
			SchemaNodeID: celunstructured.UnstructuredToVal(schemaData, &openapi.Schema{Schema: schemaVarSchema}),
		}))
	}
	rtOpts = append(rtOpts, runtime.WithNodeObjectOverride(SchemaNodeID, &unstructured.Unstructured{Object: schemaData}))
	rtOpts = append(rtOpts, opts...)
	rt := runtime.New(prog, g, rtOpts...)
	return rt, g, nil
}

// validateBuildInputs enforces the non-nil contract shared by both build
// entrypoints. The messages are part of the instance controller's surfaced
// conditions, so keep them stable.
func validateBuildInputs(rgd *v1alpha1.ResourceGraphDefinition, instance *unstructured.Unstructured, c Compiler) error {
	if rgd == nil {
		return fmt.Errorf("rgdadapter: rgd is required")
	}
	if instance == nil {
		return fmt.Errorf("rgdadapter: instance is required")
	}
	if c == nil {
		return fmt.Errorf("rgdadapter: compiler is required")
	}
	return nil
}

// stampGraphMeta sets the Graph's name/namespace from the instance so the
// executor can default namespaced resources to the instance's namespace
// (it reads rt.Graph().GetNamespace()). ObjectMeta is deliberately excluded
// from the compile-cache hash, which fingerprints GraphSpec only.
func stampGraphMeta(g *v1alpha1.Graph, instance *unstructured.Unstructured) {
	g.ObjectMeta = metav1.ObjectMeta{
		Name:      instance.GetName(),
		Namespace: instance.GetNamespace(),
	}
}

// schemaFingerprint returns a deterministic sha256 hex fingerprint of the RGD's
// declared schema. Returns "" when s is nil.
func schemaFingerprint(s *v1alpha1.Schema) string {
	if s == nil {
		return ""
	}
	data, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// schemaCompileOpts builds the CompileOptions shared by both build paths:
//   - the `schema` def node is marked literal so instance data is not parsed
//     as CEL expressions;
//   - its type is taken from the RGD's declared SimpleSchema (override), not
//     inferred from the current instance value, so a fresh instance missing
//     fields must still compile;
//   - a synthesized author-status writeback node is marked soft-deps (never
//     gates on the resources it reads) and per-field data-pending-tolerant so
//     status projects progressively and non-gating, mirroring
//     ProjectInstanceStatus.
func schemaCompileOpts(rgd *v1alpha1.ResourceGraphDefinition, g *v1alpha1.Graph) ([]compiler.CompileOption, error) {
	opts := []compiler.CompileOption{compiler.WithLiteralNode(SchemaNodeID)}
	if rgd.Spec.Schema != nil {
		schemaVarSchema, err := graph.InstanceSchemaForCEL(rgd)
		if err != nil {
			return nil, fmt.Errorf("rgdadapter: instance schema: %w", err)
		}
		opts = append(opts, compiler.WithNodeSchemaOverride(SchemaNodeID, schemaVarSchema))
	}
	for i := range g.Spec.Nodes {
		if g.Spec.Nodes[i].ID == StatusPatchNodeID && g.Spec.Nodes[i].Patch != nil {
			opts = append(opts,
				compiler.WithSoftDependencies(StatusPatchNodeID),
				compiler.WithDataPendingTolerant(StatusPatchNodeID))
			break
		}
	}
	return opts, nil
}
