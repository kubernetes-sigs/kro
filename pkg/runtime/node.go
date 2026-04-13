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

package runtime

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/pkg/metrics"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/runtime/resolver"
)

// Node is the mutable runtime handle that wraps an immutable graph.Node.
// Each reconciliation creates fresh Node instances.
type Node struct {
	Spec *graph.Node

	// deps holds pointers to only the nodes this node depends on.
	// Includes "schema" pointing to the instance node for schema.* expressions.
	deps map[string]*Node

	desired  []*unstructured.Unstructured
	observed []*unstructured.Unstructured

	includeWhenExprs []*expressionEvaluationState
	readyWhenExprs   []*expressionEvaluationState
	forEachExprs     []*expressionEvaluationState
	templateExprs    []*expressionEvaluationState
	templateVars     []*variable.ResourceField

	rgdConfig graph.RGDConfig

	// resourceSchema is the OpenAPI schema for this node's resource type.
	// Used by buildContext to wrap observed resources with schema-aware CEL values.
	resourceSchema *spec.Schema
}

// defaultIdentityPaths are the template field paths used to identify most resource types.
var defaultIdentityPaths = []string{"metadata.name", "metadata.namespace"}

// identityPathsOverride specifies non-default identity paths for specific node types.
// Most resources use name and namespace; only special cases need an override.
var identityPathsOverride = map[graph.NodeType][]string{
	// External collections have no name; the selector is their identity.
	graph.NodeTypeExternalCollection: {"metadata.namespace", "metadata.selector"},
}

// identityPathsForNodeType returns the template field path prefixes that should
// be resolved when getting a node's identity for observation or deletion.
func identityPathsForNodeType(nodeType graph.NodeType) []string {
	if override, ok := identityPathsOverride[nodeType]; ok {
		return override
	}
	return defaultIdentityPaths
}

// resolveMode controls how template resolution behaves.
type resolveMode int

const (
	// resolveFull evaluates all vars, fails on pending, caches result.
	// Used by GetDesired for normal reconciliation.
	resolveFull resolveMode = iota

	// resolveIdentity evaluates identity paths only, no dep readiness check, no cache.
	// Used by GetDesiredIdentity for deletion/observation.
	resolveIdentity
)

// IsIgnored reports whether this node should be skipped entirely.
// It is true when:
//   - any dependency is ignored (contagious)
//   - any includeWhen expression evaluates to false
//
// Results are memoized via expression caching - once an includeWhen
// expression evaluates to false, it stays false for this runtime instance.
func (n *Node) IsIgnored() (bool, error) {
	// Instance nodes cannot be ignored - they represent the user's CR.
	if n.Spec.Meta.Type == graph.NodeTypeInstance {
		return false, nil
	}

	metrics.NodeIgnoredCheckTotal.Inc()

	// Check if any dependency is ignored (contagious).
	for _, dep := range n.deps {
		ignored, err := dep.IsIgnored()
		if err != nil {
			return false, err
		}
		if ignored {
			metrics.NodeIgnoredTotal.Inc()
			return true, nil
		}
	}

	if len(n.includeWhenExprs) == 0 {
		return false, nil
	}

	needed := make(map[string]struct{})
	resourceRefs := make(map[string]struct{})
	for _, expr := range n.includeWhenExprs {
		for _, ref := range expr.Expression.References {
			needed[ref] = struct{}{}
			if ref != graph.InstanceNodeID {
				resourceRefs[ref] = struct{}{}
			}
		}
	}

	// Resource-backed includeWhen conditions evaluate against observed upstream
	// state, so they must wait until those dependencies are ready. The caller
	// already checked contagious ignore propagation above, so only inspect the
	// dependency's observed readiness here.
	for depID := range resourceRefs {
		dep, ok := n.deps[depID]
		if !ok {
			return false, fmt.Errorf("includeWhen dependency %q not wired into runtime", depID)
		}
		err := dep.checkObservedReadiness()
		if errors.Is(err, ErrWaitingForReadiness) {
			return false, fmt.Errorf("includeWhen dependency %q not ready: %s (%w)", depID, err.Error(), ErrDataPending)
		}
		if err != nil {
			return false, fmt.Errorf("includeWhen dependency %q: %w", depID, err)
		}
	}

	ctx := n.buildContext(slices.Collect(maps.Keys(needed))...)

	for _, expr := range n.includeWhenExprs {
		hasResourceRef := false
		for _, ref := range expr.Expression.References {
			if ref != graph.InstanceNodeID {
				hasResourceRef = true
				break
			}
		}
		val, err := evalBoolExpr(expr, ctx)
		if err != nil {
			if hasResourceRef && isCELDataPending(err) {
				return false, fmt.Errorf("includeWhen %q: %w (%w)", expr.Expression.UserExpression(), err, ErrDataPending)
			}
			return false, fmt.Errorf("includeWhen %q: %w", expr.Expression.UserExpression(), err)
		}
		if !val {
			metrics.NodeIgnoredTotal.Inc()
			return true, nil
		}
	}

	return false, nil
}

// GetDesired computes and returns the desired state(s) for this node.
// Results are cached - subsequent calls return the cached value.
// Behavior varies by node type:
//   - Resource: strict evaluation, fails fast on any error
//   - Collection: strict evaluation with forEach expansion
//   - Instance: best-effort partial evaluation
//   - External: resolves template (for name/namespace CEL), caller reads instead of applies
//
// Note: The caller should call IsIgnored() before GetDesired() for resource nodes.
func (n *Node) GetDesired() ([]*unstructured.Unstructured, error) {
	// Return cached result if available.
	if n.desired != nil {
		return n.desired, nil
	}
	return n.resolve(resolveFull)
}

// GetDesiredIdentity resolves only identity-related fields (metadata.name & namespace)
// and skips readiness gating. It is used for deletion/observation when we only need
// stable identities and want to avoid being blocked by unrelated template fields.
//
// NOTE: This method does not cache its result in n.desired; callers in non-deletion
// paths should continue using GetDesired().
func (n *Node) GetDesiredIdentity() ([]*unstructured.Unstructured, error) {
	return n.resolve(resolveIdentity)
}

// resolve is the unified resolution method. The mode controls which vars are
// resolved, whether to tolerate pending dependencies, and whether to cache.
func (n *Node) resolve(mode resolveMode) (result []*unstructured.Unstructured, err error) {
	startTime := time.Now()
	defer func() {
		duration := time.Since(startTime)
		metrics.NodeEvalDuration.Observe(duration.Seconds())
		metrics.NodeEvalTotal.Inc()
		if err != nil {
			metrics.NodeEvalErrorsTotal.Inc()
		}
	}()

	// Full and partial modes check dep readiness (partial skips for instance nodes).
	if mode == resolveFull && n.Spec.Meta.Type != graph.NodeTypeInstance {
		for depID, dep := range n.deps {
			if depID == graph.InstanceNodeID {
				continue
			}
			err := dep.CheckReadiness()
			if errors.Is(err, ErrWaitingForReadiness) {
				return nil, fmt.Errorf("node %q: dependent node %q not ready: %s (%w)", n.Spec.Meta.ID, dep.Spec.Meta.ID, err.Error(), ErrDataPending)
			}
			if err != nil {
				return nil, fmt.Errorf("node %q: failed to check readiness of dependent node %q: %w", n.Spec.Meta.ID, dep.Spec.Meta.ID, err)
			}
		}
	}

	// Select vars based on mode.
	vars := n.templateVars
	if mode == resolveIdentity {
		vars = n.templateVarsForPaths(identityPathsForNodeType(n.Spec.Meta.Type))
	}

	switch n.Spec.Meta.Type {
	case graph.NodeTypeInstance:
		if mode == resolveIdentity {
			panic("GetDesiredIdentity called for instance node")
		}
		result, err = n.softResolve()
	case graph.NodeTypeCollection:
		setIndexLabel := mode == resolveFull
		result, err = n.hardResolveCollection(vars, setIndexLabel)
	case graph.NodeTypeResource, graph.NodeTypeExternal:
		result, err = n.hardResolveSingleResource(vars)
	case graph.NodeTypeExternalCollection:
		result, err = n.hardResolveSingleResource(vars)
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}

	if err != nil {
		return nil, err
	}

	// Normalize namespaces unless an external collection is intentionally using
	// an empty namespace to list across all namespaces.
	if n.Spec.Meta.Type != graph.NodeTypeInstance && n.Spec.Meta.Type != graph.NodeTypeExternalCollection {
		if err = n.normalizeNamespaces(result); err != nil {
			return nil, err
		}
	}

	if mode != resolveIdentity {
		n.desired = result
	}

	return result, nil
}

// DeleteTargets returns the ordered list of objects this node should delete now.
//
// This is intentionally narrow today: it only reasons about identity resolution
// and currently observed objects, and returns the safe deletion targets. It is the
// runtime's deletion gate so callers don't re-implement matching logic.
//
// Long-term, this should evolve into an ActionPlan where the runtime tells the
// caller which resources to create, update, keep intact, or delete, and where
// propagation/rollout gates are enforced in one place.
func (n *Node) DeleteTargets() ([]*unstructured.Unstructured, error) {
	switch n.Spec.Meta.Type {
	case graph.NodeTypeCollection, graph.NodeTypeResource:
		desired, err := n.GetDesiredIdentity()
		if err != nil {
			return nil, err
		}
		if n.Spec.Meta.Type == graph.NodeTypeCollection {
			return orderedIntersection(n.observed, desired), nil
		}
		return n.observed, nil
	case graph.NodeTypeInstance, graph.NodeTypeExternal, graph.NodeTypeExternalCollection:
		panic(fmt.Sprintf("DeleteTargets called for node type %v", n.Spec.Meta.Type))
	default:
		panic(fmt.Sprintf("unknown node type: %v", n.Spec.Meta.Type))
	}
}

// upsertToTemplate applies values by upserting at paths, creating parent fields if needed.
// Use for instance status where paths like status.foo may not exist yet.
func (n *Node) upsertToTemplate(base *unstructured.Unstructured, values map[string]any) *unstructured.Unstructured {
	desired := base.DeepCopy()
	res := resolver.NewResolver(desired.Object, values)
	for _, v := range n.templateVars {
		if val, ok := values[v.Expression.Original]; ok {
			_ = res.UpsertValueAtPath(v.Path, val)
		}
	}
	return desired
}

// SetObserved stores the observed state(s) from the cluster.
func (n *Node) SetObserved(observed []*unstructured.Unstructured) {
	switch n.Spec.Meta.Type {
	case graph.NodeTypeCollection:
		n.observed = orderedIntersection(observed, n.desired)
	case graph.NodeTypeExternalCollection:
		// External collections store all observed items directly; there is
		// no desired set to intersect with.
		n.observed = observed
	default:
		n.observed = observed
	}
}
