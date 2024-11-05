// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package parser

import (
	"errors"
	"strings"
)

const (
	// In symphony, CEL expressions are enclosed between "${" and "}"
	exprStart = "${"
	exprEnd   = "}"
)

var ErrNestedExpression = errors.New("nested expressions are not allowed")

// extractExpressions extracts all non-nested CEL expressions from a string.
// It returns an error if it encounters a nested expression.
func extractExpressions(str string) ([]string, error) {
	var expressions []string

	start := 0
	// Iterate over the string and find all expressions
	for start < len(str) {
		// Find the start of the next expression. If none is found, break
		startIdx := strings.Index(str[start:], exprStart)
		if startIdx == -1 {
			break
		}
		// Adjust the start index to the actual position in the string
		startIdx += start

		// We need to find the matching end bracket. we have to be careful about
		// nested expressions and dictionary building expressions. For example:
		// a user can have an expression like "${{"key": 123}}". For this reason,
		// we need to keep track of the bracket count and make sure that we only
		// consider the outermost expression
		bracketCount := 1
		endIdx := startIdx + len(exprStart)
		for endIdx < len(str) {
			if str[endIdx] == '{' {
				bracketCount++
			} else if str[endIdx] == '}' {
				bracketCount--
				// If we have reached the end of the expression, break
				if bracketCount == 0 {
					break
				}
			} else if endIdx+1 < len(str) && str[endIdx:endIdx+2] == "${" {
				// We do not allow nested expressions. I'm not sure if this is a
				// good idea, but its sounds like a reasonable restriction.
				return nil, ErrNestedExpression
			}
			endIdx++
		}

		if bracketCount != 0 {
			// Incomplete expression, move to next character and continue
			start++
			continue
		}

		// The expression is the substring between the start and end indices
		// of '${' and the matching '}'
		expr := str[startIdx+len(exprStart) : endIdx]
		expressions = append(expressions, expr)
		start = endIdx + 1
	}
	return expressions, nil
}

// isStandaloneExpression returns true if the string is a single, complete non-nested expression.
// It returns an error if it encounters a nested expression.
func isStandaloneExpression(str string) (bool, error) {
	expressions, err := extractExpressions(str)
	if err != nil {
		return false, err
	}

	return len(expressions) == 1 && str == exprStart+expressions[0]+exprEnd, nil
}
