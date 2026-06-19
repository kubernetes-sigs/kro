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

package resolver

import (
	"fmt"
	"net/http"

	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// NewCombinedResolver wires a two-tier schema resolver:
//
//   - CoreResolver answers from compiled-in OpenAPI definitions
//     (built-in types: Pod, Deployment, etc.). Stable per apiserver
//     binary; no caching needed.
//   - CachedSchemaResolver wraps a discovery-backed resolver for CRDs
//     with an LRU cache + push-driven invalidation. The schema
//     watcher invalidates entries when CRD content changes; the
//     cache otherwise holds indefinitely.
//
// The combined resolver tries Core first, falls through to Cached on
// miss. Returns the live CachedSchemaResolver alongside the combined
// view so callers can plumb invalidations through to it.
func NewCombinedResolver(clientConfig *rest.Config, httpClient *http.Client) (resolver.SchemaResolver, *CachedSchemaResolver, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfigAndClient(clientConfig, httpClient)
	if err != nil {
		return nil, nil, fmt.Errorf("create discovery client: %w", err)
	}

	clientResolver := &resolver.ClientDiscoveryResolver{
		Discovery: discoveryClient,
	}

	// 500 entries comfortably covers ~200 CRDs × 2-3 versions, with
	// LRU eviction handling installations with more.
	cached, err := NewCachedSchemaResolver(clientResolver, 500)
	if err != nil {
		return nil, nil, fmt.Errorf("create cached schema resolver: %w", err)
	}

	coreResolver := resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)

	combined := coreResolver.Combine(cached)
	return combined, cached, nil
}
