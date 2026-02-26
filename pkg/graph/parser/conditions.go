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

package parser

import (
	"fmt"
	"strings"
)

// ParseConditionExpressions parses resource condition expressions (readyWhen, includeWhen).
// These must be standalone expressions (${...}). Returns raw expression strings.
func ParseConditionExpressions(conditions []string) ([]string, error) {
	expressions := make([]string, 0, len(conditions))

	for _, e := range conditions {
		ok, err := isStandaloneExpression(e)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("only standalone expressions are allowed")
		}
		expr := strings.TrimPrefix(e, "${")
		expr = strings.TrimSuffix(expr, "}")
		expressions = append(expressions, expr)
	}

	return expressions, nil
}
