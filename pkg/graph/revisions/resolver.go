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

type rgdResolver struct {
	registry *Registry
	rgdName  string
}

// ResolverForRGD returns a resolver scoped to a single RGD name.
func (r *Registry) ResolverForRGD(rgdName string) rgdResolver {
	return rgdResolver{
		registry: r,
		rgdName:  rgdName,
	}
}

// GetLatestRevision returns the newest cached revision for this resolver's RGD.
func (r rgdResolver) GetLatestRevision() (Entry, bool) {
	return r.registry.Latest(r.rgdName)
}

// GetGraphRevision returns a specific cached revision for this resolver's RGD.
func (r rgdResolver) GetGraphRevision(revision int64) (Entry, bool) {
	return r.registry.Get(r.rgdName, revision)
}
