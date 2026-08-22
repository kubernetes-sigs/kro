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
	// ApplyOrderAnnotation persists a managed resource's reverse topological deletion wave.
	ApplyOrderAnnotation = InternalKROPrefix + "apply-order"
	// PatchContributionsAnnotation persists the inventory of patch-node
	// field-manager contributions on a Graph, as a JSON array. It drives
	// release-on-prune: contributions present last reconcile but absent this
	// one have their fields released under their field manager.
	PatchContributionsAnnotation = InternalKROPrefix + "patch-contributions"
)
