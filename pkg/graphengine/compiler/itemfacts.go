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
	"fmt"

	"github.com/google/cel-go/cel"
	celast "github.com/google/cel-go/common/ast"
)

// itemFactFunctions are the member functions defined only on the items of a
// per-item gate: a collection's own items (via the collection id or `each`)
// and, on the RGD fleet gate, `instances` / `each`. They are meaningless on
// anything else: a single resource is always at its current render when a
// dependent reads it, so updated() would be vacuously true, and ready() on a
// node is spelled <id>.ready() and lowers to the hidden readiness map.
var itemFactFunctions = map[string]struct{}{"updated": {}, "ready": {}}

// CheckItemFactReceivers rejects updated()/ready() calls whose
// receiver is not an item of one of allowedRoots. Comprehension variables are
// resolved to the root of their iteration range, so `web.filter(w, w.updated())`
// is accepted when "web" is allowed and `other.filter(o, o.ready())` is
// rejected.
//
// nodeIDs, when non-nil, enables the node form: `<id>.ready()` with a bare node
// ident as receiver is a node readiness read (lowered by NodeReadyMacro), not
// an item fact. It is accepted for any node in nodeIDs other than self, and
// the referenced ids are returned so the caller can add dependency edges.
// `<id>.updated()` on a node ident is always rejected: a single resource is at
// its current render whenever a dependent reads it.
func CheckItemFactReceivers(env *cel.Env, expr string, allowedRoots map[string]struct{}) error {
	_, err := CheckFactReceivers(env, expr, allowedRoots, nil, "")
	return err
}

// CheckFactReceivers is CheckItemFactReceivers plus the node form; see there.
func CheckFactReceivers(env *cel.Env, expr string, allowedRoots map[string]struct{}, nodeIDs map[string]bool, self string) ([]string, error) {
	parsed, iss := env.Parse(expr)
	if iss.Err() != nil {
		return nil, nil // type-check reports parse errors with better context
	}
	w := &factWalker{allowed: allowedRoots, bound: map[string]string{}, nodeIDs: nodeIDs, self: self}
	if err := w.walk(parsed.NativeRep().Expr()); err != nil {
		return nil, err
	}
	return w.nodeReads, nil
}

type factWalker struct {
	allowed map[string]struct{}
	// bound maps a comprehension variable to the root identifier of its range,
	// or "" when the range is not rooted at an identifier.
	bound map[string]string
	// nodeIDs enables `<id>.ready()` on a bare node ident; self is rejected.
	nodeIDs   map[string]bool
	self      string
	nodeReads []string
}

func (w *factWalker) root(e celast.Expr) string {
	switch e.Kind() {
	case celast.IdentKind:
		name := e.AsIdent()
		if r, ok := w.bound[name]; ok {
			return r
		}
		return name
	case celast.SelectKind:
		return w.root(e.AsSelect().Operand())
	case celast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() && c.Target() != nil {
			return w.root(c.Target())
		}
	case celast.ComprehensionKind:
		return w.root(e.AsComprehension().IterRange())
	}
	return ""
}

func (w *factWalker) walk(e celast.Expr) error {
	switch e.Kind() {
	case celast.CallKind:
		c := e.AsCall()
		if c.IsMemberFunction() && c.Target() != nil {
			if _, isFact := itemFactFunctions[c.FunctionName()]; isFact && len(c.Args()) == 0 {
				// Bare node ident receiver (not a comprehension variable).
				if t := c.Target(); t.Kind() == celast.IdentKind && w.nodeIDs != nil {
					if name := t.AsIdent(); w.nodeIDs[name] {
						if _, isBound := w.bound[name]; !isBound {
							if c.FunctionName() != "ready" {
								return fmt.Errorf("%s() is only defined on the items of a gated collection; a node is at its current render whenever a dependent reads it", c.FunctionName())
							}
							if name == w.self {
								return fmt.Errorf("%s.ready() in its own propagateWhen can never be satisfied", name)
							}
							w.nodeReads = append(w.nodeReads, name)
							return nil
						}
					}
				}
				r := w.root(c.Target())
				if _, ok := w.allowed[r]; !ok {
					return w.factError(c.FunctionName(), r)
				}
			}
			if err := w.walk(c.Target()); err != nil {
				return err
			}
		}
		for _, a := range c.Args() {
			if err := w.walk(a); err != nil {
				return err
			}
		}
	case celast.ComprehensionKind:
		comp := e.AsComprehension()
		if err := w.walk(comp.IterRange()); err != nil {
			return err
		}
		r := w.root(comp.IterRange())
		prev, had := w.bound[comp.IterVar()]
		w.bound[comp.IterVar()] = r
		var prev2 string
		var had2 bool
		if v2 := comp.IterVar2(); v2 != "" {
			prev2, had2 = w.bound[v2]
			w.bound[v2] = r
		}
		for _, sub := range []celast.Expr{comp.AccuInit(), comp.LoopCondition(), comp.LoopStep(), comp.Result()} {
			if err := w.walk(sub); err != nil {
				return err
			}
		}
		if had {
			w.bound[comp.IterVar()] = prev
		} else {
			delete(w.bound, comp.IterVar())
		}
		if v2 := comp.IterVar2(); v2 != "" {
			if had2 {
				w.bound[v2] = prev2
			} else {
				delete(w.bound, v2)
			}
		}
	case celast.SelectKind:
		return w.walk(e.AsSelect().Operand())
	case celast.ListKind:
		for _, el := range e.AsList().Elements() {
			if err := w.walk(el); err != nil {
				return err
			}
		}
	case celast.MapKind:
		for _, en := range e.AsMap().Entries() {
			me := en.AsMapEntry()
			if err := w.walk(me.Key()); err != nil {
				return err
			}
			if err := w.walk(me.Value()); err != nil {
				return err
			}
		}
	case celast.StructKind:
		for _, f := range e.AsStruct().Fields() {
			if err := w.walk(f.AsStructField().Value()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *factWalker) factError(fn, receiver string) error {
	if receiver == "" {
		return fmt.Errorf("%s() is only defined on the items of a gated collection", fn)
	}
	if fn == "ready" {
		return fmt.Errorf("%s() is only defined on the items of a gated collection or on a node of this graph; %q is neither", fn, receiver)
	}
	return fmt.Errorf("%s() is only defined on the items of a gated collection; a single resource is always at its current render when a dependent reads it", fn)
}
