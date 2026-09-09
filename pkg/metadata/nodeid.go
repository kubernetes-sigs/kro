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

package metadata

import (
	"strings"
)

// NodeIDToken returns a bounded, label-safe rendering of a node's qualified
// path for the NodeIDLabel value and the collection watch selector.
//
// Node IDs are strictly alphanumeric (^[A-Za-z][A-Za-z0-9]*$), so '.' is an
// unambiguous, reversible frame separator: the '.'-joined path (e.g.
// "subA.res") is a valid label value and round-trips to the '/'-form. At the
// root the token is just the bare node ID, preserving the documented
// `kubectl get -l kro.run/node-id=<id>` query for top-level nodes.
//
// When the '.'-joined path would exceed the 63-char label-value limit (deep or
// long-named nesting) it is replaced by a stable, collision-resistant hash so
// the label stays valid at any depth. The full readable path is always
// preserved in the NodePathAnnotation regardless, so a hashed label never
// costs debuggability. Selectors are built from this same function, so they
// match the stamped label by construction.
func NodeIDToken(qualifiedPath string) string {
	if !NodeIDTokenIsHashed(qualifiedPath) {
		return dottedNodePath(qualifiedPath)
	}
	// Hash the '/'-form, not the dotted one, so "a/b" and "a.b" stay distinct.
	return hashedLabelValue(qualifiedPath)
}

// NodeIDTokenIsHashed reports whether NodeIDToken has to hash this qualified
// path because its label-safe rendering does not fit in a label value.
func NodeIDTokenIsHashed(qualifiedPath string) bool {
	return LabelValueIsHashed(dottedNodePath(qualifiedPath))
}

func dottedNodePath(qualifiedPath string) string {
	return strings.ReplaceAll(qualifiedPath, "/", ".")
}
