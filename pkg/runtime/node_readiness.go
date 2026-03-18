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

	"github.com/kubernetes-sigs/kro/pkg/graph"
)

// CheckReadiness evaluates readyWhen expressions using observed state.
// Ignored nodes are treated as ready for dependency gating purposes.
func (n *Node) CheckReadiness() error {
	nodeReadyCheckTotal.Inc()

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
		nodeNotReadyTotal.Inc()
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
		return fmt.Errorf("node %q: no observed state: %w", n.Spec.Meta.ID, ErrWaitingForReadiness)
	}
	if len(n.readyWhenExprs) == 0 {
		return nil
	}

	nodeID := n.Spec.Meta.ID
	ctx := map[string]any{nodeID: n.observed[0].Object}

	for _, expr := range n.readyWhenExprs {
		result, err := evalBoolExpr(expr, ctx)
		if err != nil {
			if isCELDataPending(err) {
				return fmt.Errorf("node %q: failed to evaluate readyWhen expression: %q (%w)", n.Spec.Meta.ID, expr.Expression.UserExpression(), ErrWaitingForReadiness)
			}
			return fmt.Errorf("node %q: failed to evaluate readyWhen expression: %q (%w)", n.Spec.Meta.ID, expr.Expression.UserExpression(), err)
		}
		if !result {
			return fmt.Errorf("readyWhen condition evaluated to false: %q (%w)", expr.Expression.UserExpression(), ErrWaitingForReadiness)
		}
	}
	return nil
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
			return fmt.Errorf("node %q: collection not computed (%w)", n.Spec.Meta.ID, ErrWaitingForReadiness)
		}
		if len(n.desired) == 0 {
			return nil
		}
		if len(n.observed) < len(n.desired) {
			return fmt.Errorf("node %q: collection not ready: observed %d but desired %d (%w)", n.Spec.Meta.ID, len(n.observed), len(n.desired), ErrWaitingForReadiness)
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
					return fmt.Errorf("node %q: failed to evaluate readyWhen %q (item %d) (%w)", n.Spec.Meta.ID, expr.Expression.UserExpression(), i, ErrWaitingForReadiness)
				}
				return fmt.Errorf("node %q: failed to evaluate readyWhen %q (item %d): %w", n.Spec.Meta.ID, expr.Expression.UserExpression(), i, err)
			}
			result, ok := val.(bool)
			if !ok {
				return fmt.Errorf("readyWhen %q did not return bool", expr.Expression.UserExpression())
			}
			if !result {
				return fmt.Errorf("readyWhen condition evaluated to false: %q (%w)", expr.Expression.UserExpression(), ErrWaitingForReadiness)
			}
		}
	}

	return nil
}
