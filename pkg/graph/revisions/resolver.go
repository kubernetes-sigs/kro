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

package revisions

// Resolver resolves compiled graph revisions for a single owner. Implementations
// are already scoped to one owner, so callers ask only for the latest issued
// revision or a specific revision number.
type Resolver interface {
	GetLatestRevision() (Entry, bool)
	GetGraphRevision(revision int64) (Entry, bool)
}

type rgdResolver struct {
	registry *Registry
	owner    OwnerKey
}

// ResolverFor returns a resolver scoped to a single owner key.
func (r *Registry) ResolverFor(owner OwnerKey) Resolver {
	return rgdResolver{registry: r, owner: owner}
}

// GetLatestRevision returns the newest cached revision for this resolver's owner.
func (r rgdResolver) GetLatestRevision() (Entry, bool) {
	return r.registry.Latest(r.owner)
}

// GetGraphRevision returns a specific cached revision for this resolver's owner.
func (r rgdResolver) GetGraphRevision(revision int64) (Entry, bool) {
	return r.registry.Get(r.owner, revision)
}
