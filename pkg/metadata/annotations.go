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

const (
	// ApplyOrderAnnotation persists a managed resource's reverse topological
	// deletion wave, as a one-based layer: dependency-free (leaf) resources are
	// wave 1 and each dependent is 1 + max(dependency waves). Deletion proceeds
	// highest-wave-first. The value is advisory (it records the computed layer);
	// only the relative ordering is contractual.
	ApplyOrderAnnotation = InternalKROPrefix + "apply-order"
	// PatchContributionsAnnotation persists the inventory of patch-node
	// field-manager contributions on a Graph, as a JSON array. It drives
	// release-on-prune: contributions present last reconcile but absent this
	// one have their fields released under their field manager.
	PatchContributionsAnnotation = InternalKROPrefix + "patch-contributions"
	// NodePathAnnotation records the fully-qualified, human-readable node path
	// of a managed resource, using '/' as the frame separator (e.g.
	// "subA/res" for node "res" declared inside subgraph node "subA").
	//
	// The kro.run/node-id LABEL cannot always hold this value: label values
	// are bounded to 63 chars and may not contain '/'. For nested nodes the
	// label therefore carries a bounded, label-safe TOKEN (the '.'-joined path
	// when it fits, otherwise a hash) while this annotation always preserves
	// the full readable path for display, debugging, and reverse lookup.
	NodePathAnnotation = InternalKROPrefix + "node-path"
)
