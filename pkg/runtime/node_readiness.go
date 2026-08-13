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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/metrics"
)

// CheckReadiness evaluates readyWhen expressions using observed state.
// Ignored nodes are treated as ready for dependency gating purposes.
func (n *Node) CheckReadiness() error {
	metrics.NodeReadyCheckTotal.Inc()

	// Ignored nodes are satisfied for dependency gating - dependents shouldn't block.
	ignored, err := n.IsIgnored()
	if err != nil {
		return fmt.Errorf("is ignore check failed: %w", err)
	}
	if ignored {
		return nil
	}

	err = n.checkObservedReadiness()
	if err != nil && errors.Is(err, ErrWaitingForReadiness) {
		metrics.NodeNotReadyTotal.Inc()
	}

	return err
}

// checkObservedReadiness evaluates readiness from the node's own observed and
// desired state. Callers that need ignore semantics must handle them first.
func (n *Node) checkObservedReadiness() error {
	if n.Spec.Meta.Type == graph.NodeTypeCollection || n.Spec.Meta.Type == graph.NodeTypeExternalCollection {
		return n.checkCollectionReadiness()
	}
	return n.checkSingleResourceReadiness()
}

func (n *Node) checkSingleResourceReadiness() error {
	if len(n.observed) == 0 {
		return newWaitingForReadinessError("node %q: no observed state", n.Spec.Meta.ID)
	}
	if len(n.readyWhenExprs) == 0 {
		return nil
	}

	nodeID := n.Spec.Meta.ID
	observed := n.observed[0]
	ctx := map[string]any{nodeID: observed.Object}

	for _, expr := range n.readyWhenExprs {
		result, err := evalBoolExpr(expr, ctx)
		if err != nil {
			if isCELDataPending(err) {
				return newWaitingForReadinessError("node %q: failed to evaluate readyWhen expression: %q", n.Spec.Meta.ID, expr.Expression.UserExpression())
			}
			return fmt.Errorf("node %q: failed to evaluate readyWhen expression: %q (%w)", n.Spec.Meta.ID, expr.Expression.UserExpression(), err)
		}
		if !result {
			return newWaitingForReadinessError("readyWhen condition evaluated to false: %q (resource: %s)", expr.Expression.UserExpression(), resourceIdentity(observed))
		}
	}
	return nil
}

// resourceIdentity formats group/version/kind and namespace/name for a
// resource so readiness messages can point directly at the object being
// waited on, without requiring knowledge of the RGD's internal node IDs.
func resourceIdentity(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return fmt.Sprintf("%s %s/%s", obj.GroupVersionKind().String(), ns, obj.GetName())
	}
	return fmt.Sprintf("%s %s", obj.GroupVersionKind().String(), obj.GetName())
}

func (n *Node) checkCollectionReadiness() error {
	if n.Spec.Meta.Type == graph.NodeTypeExternalCollection {
		// External collections: desired carries the selector template, not actual
		// desired resources. Skip count-based readiness checks.
		if len(n.readyWhenExprs) == 0 || len(n.observed) == 0 {
			return nil
		}
	} else {
		// Use nil check (not len==0) to distinguish "not computed" from "empty collection".
		if n.desired == nil {
			return newWaitingForReadinessError("node %q: collection not computed", n.Spec.Meta.ID)
		}
		if len(n.desired) == 0 {
			return nil
		}
		if len(n.observed) < len(n.desired) {
			return newWaitingForReadinessError("node %q: collection not ready: observed %d but desired %d", n.Spec.Meta.ID, len(n.observed), len(n.desired))
		}
		if len(n.readyWhenExprs) == 0 {
			return nil
		}
	}

	// Collection readyWhen uses "each" (single item) only.
	// Each item has different context, so we evaluate directly (not cached).
	for i, obj := range n.observed {
		ctx := map[string]any{graph.EachVarName: obj.Object}
		for _, expr := range n.readyWhenExprs {
			// readyWhen for collections must NOT be cached - each item has different "each" context.
			// Use Expression.Eval directly instead of evalBoolExpr.
			val, err := expr.Expression.Eval(ctx)
			if err != nil {
				if isCELDataPending(err) {
					return newWaitingForReadinessError("node %q: failed to evaluate readyWhen %q (item %d)", n.Spec.Meta.ID, expr.Expression.UserExpression(), i)
				}
				return fmt.Errorf("node %q: failed to evaluate readyWhen %q (item %d): %w", n.Spec.Meta.ID, expr.Expression.UserExpression(), i, err)
			}
			result, ok := val.(bool)
			if !ok {
				return fmt.Errorf("readyWhen %q did not return bool", expr.Expression.UserExpression())
			}
			if !result {
				return newWaitingForReadinessError("readyWhen condition evaluated to false: %q (resource: %s)", expr.Expression.UserExpression(), resourceIdentity(obj))
			}
		}
	}

	return nil
}
