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

package graph

import "github.com/kubernetes-sigs/kro/pkg/graph/parser"

// ParseStatusExpressions is a thin exported wrapper around the package's
// unexported extractConditionExpressions and the parser's ParseSchemalessResource,
// exposed so graph consumers can project instance status without duplicating
// parsing logic or importing unexported symbols.
//
// It takes a pre-unmarshalled status map (from RGD.Spec.Schema.Status.Raw),
// mutates it to remove the `conditions:` key, and returns:
//   - fields: StatusFieldExpr list (path → raw CEL expression) for non-condition fields
//   - conditionExprs: the raw "${...}" condition expression strings
//   - noExprFields: paths that carry no CEL expression (callers may treat these as errors)
//
// The caller owns the returned slices. The input map is mutated: the
// `conditions` key is removed when present.
func ParseStatusExpressions(statusMap map[string]interface{}) (
	fields []StatusFieldExpr,
	conditionExprs []string,
	noExprFields []string,
	err error,
) {
	// Remove and return the conditions block first (mutates statusMap in-place),
	// matching what inferStatusSchema does before calling parser.ParseSchemalessResource.
	conditionExprs, err = extractConditionExpressions(statusMap)
	if err != nil {
		return nil, nil, nil, err
	}

	fds, noExpr, err := parser.ParseSchemalessResource(statusMap)
	if err != nil {
		return nil, nil, nil, err
	}

	fields = make([]StatusFieldExpr, len(fds))
	for i, fd := range fds {
		fields[i] = StatusFieldExpr{
			Path:       fd.Path,
			Expression: fd.Expression.Original,
		}
	}
	return fields, conditionExprs, noExpr, nil
}

// StatusFieldExpr is the minimal representation of a status field a graph
// consumer needs: a JSON-path location and the raw (unwrapped, inner) CEL
// expression string.
type StatusFieldExpr struct {
	// Path is the dotted/bracketed JSON path of the status field
	// (e.g. "readyName" or "nested.value").
	Path string
	// Expression is the inner CEL expression (without ${...} wrappers),
	// ready to be compiled against a CEL environment.
	Expression string
}
