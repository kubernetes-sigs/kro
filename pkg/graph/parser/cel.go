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
	"errors"
	"strings"
)

const (
	// In kro, CEL expressions are enclosed between "${" and "}"
	exprStart = "${"
	exprEnd   = "}"
)

// Allow nested expressions, but only if they are escaped with quotes ${outer("${inner}")} is allowed, but ${outer(${inner})} is not
var ErrNestedExpression = errors.New("nested expressions are not allowed unless inside string literals")

// exprMatch holds a parsed CEL expression and its position in the original string.
type exprMatch struct {
	expr  string // Expression content (without ${})
	start int    // Position where ${ starts
	end   int    // Position after closing }
}

// ExtractExpressions extracts all non-nested CEL expressions from a string.
// It returns an error if it encounters a nested expression.
func ExtractExpressions(str string) ([]string, error) {
	matches, err := extractExpressions(str)
	if err != nil {
		return nil, err
	}
	exprs := make([]string, len(matches))
	for i, m := range matches {
		exprs[i] = m.expr
	}
	return exprs, nil
}

// extractExpressions extracts all non-nested CEL expressions from a string,
// returning each expression along with its start/end position.
func extractExpressions(str string) ([]exprMatch, error) {
	var matches []exprMatch

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

		// We need to find the matching end bracket, being careful about
		// nested expressions, dictionary building expressions, and string literals
		bracketCount := 1
		endIdx := startIdx + len(exprStart)
		inStringLiteral := false
		escapeNext := false

		for endIdx < len(str) {
			c := str[endIdx]

			// Handle escape sequences inside string literals
			if escapeNext {
				escapeNext = false
				endIdx++
				continue
			}

			// Check for escape character inside string literals
			if inStringLiteral && c == '\\' {
				escapeNext = true
				endIdx++
				continue
			}

			// Handle string literal boundaries
			if c == '"' {
				inStringLiteral = !inStringLiteral
			} else if !inStringLiteral {
				// Only count braces when not inside a string literal
				if c == '{' {
					bracketCount++
				} else if c == '}' {
					bracketCount--
					if bracketCount == 0 {
						break
					}
				} else if endIdx+1 < len(str) && str[endIdx:endIdx+2] == "${" {
					// Allow nested expressions, but only if they are escaped with quotes
					if str[endIdx-1] != '"' {
						return nil, ErrNestedExpression
					}
				}
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
		matches = append(matches, exprMatch{
			expr:  expr,
			start: startIdx,
			end:   endIdx + 1,
		})
		start = endIdx + 1
	}
	return matches, nil
}

// isStandaloneExpression returns true if the string is a single, complete non-nested expression.
// It returns an error if it encounters a nested expression.
func isStandaloneExpression(str string) (bool, error) {
	matches, err := extractExpressions(str)
	if err != nil {
		return false, err
	}

	return len(matches) == 1 && matches[0].start == 0 && matches[0].end == len(str), nil
}
