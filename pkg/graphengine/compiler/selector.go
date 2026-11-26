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

package compiler

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1validation "k8s.io/apimachinery/pkg/apis/meta/v1/validation"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

var selectorFieldPath = field.NewPath("metadata", "selector")

var errRefSelectorShape = fmt.Errorf("%s must be a Kubernetes LabelSelector object (matchLabels and/or matchExpressions) or a single CEL expression that resolves to one", selectorFieldPath)

// DecodeLabelSelector strictly decodes a rendered metadata.selector into a
// metav1.LabelSelector and validates it. Unknown keys are errors: dropping them
// would leave an empty selector that matches every object of the kind.
func DecodeLabelSelector(raw map[string]any) (*metav1.LabelSelector, error) {
	ls := &metav1.LabelSelector{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(raw, ls, true); err != nil {
		return nil, fmt.Errorf("%s is not a valid LabelSelector object (matchLabels: map of string to string, matchExpressions: list of {key, operator, values}): %w", selectorFieldPath, err)
	}
	if errs := metav1validation.ValidateLabelSelector(ls, metav1validation.LabelSelectorValidationOptions{}, selectorFieldPath); len(errs) != 0 {
		return nil, fmt.Errorf("invalid label selector: %w", errs.ToAggregate())
	}
	return ls, nil
}

// validateRefSelector checks a ref node's schemaless metadata.selector at
// compile time: a standalone ${...} expression is accepted (decoded at apply
// time); an object must fit the LabelSelector schema and, when it holds no CEL,
// decode strictly. An absent or null selector is not a collection ref.
func validateRefSelector(p *parser.Parser, payload map[string]any) error {
	md, _ := payload["metadata"].(map[string]any)
	sel, ok := md["selector"]
	if !ok || sel == nil {
		return nil
	}
	switch s := sel.(type) {
	case string:
		standalone, err := parser.IsStandaloneExpression(s)
		if err != nil {
			return fmt.Errorf("%s: invalid expression: %w", selectorFieldPath, err)
		}
		if !standalone {
			return errRefSelectorShape
		}
		return nil
	case map[string]any:
		expressions, err := p.ParseResourceAtPath(s, &schema.LabelSelectorSchema, selectorFieldPath.String())
		if err != nil {
			return err
		}
		if len(expressions) > 0 {
			// CEL values are only known at apply time; the executor decodes them.
			return nil
		}
		_, err = DecodeLabelSelector(s)
		return err
	default:
		return errRefSelectorShape
	}
}
