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

package compiler

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common"
	celast "github.com/google/cel-go/common/ast"
)

// NodeReadyMacro lowers `<nodeId>.ready()` in a propagateWhen expression to a
// lookup in the hidden readiness map, `__kro_ready.<nodeId>`.
//
// Why a macro rather than a member function on the node value: the readiness
// verdict is not part of the object kro publishes for the node, and stamping
// it into that object would leak into templates that copy the whole value.
// The map keeps the verdict out of the object, the compiler can check that
// the id is a node in this graph and add the dependency edge, and `.ready()`
// on anything that is NOT a node ident (a comprehension variable such as
// `w` in `web.filter(w, w.ready())`, or `each`) is left alone and resolves to
// the item fact. Authors write one spelling, `.ready()`, in both places.
func NodeReadyMacro(nodeIDs map[string]bool) cel.Macro {
	return cel.ReceiverMacro("ready", 0,
		func(eh cel.MacroExprFactory, target celast.Expr, args []celast.Expr) (celast.Expr, *common.Error) {
			if target == nil || target.Kind() != celast.IdentKind || !nodeIDs[target.AsIdent()] {
				return nil, nil
			}
			return eh.NewSelect(eh.NewIdent(ReadyVarName), target.AsIdent()), nil
		})
}
