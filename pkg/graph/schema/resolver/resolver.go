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
	"time"

	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// NewCombinedResolver creates a new schema resolver that can resolve both core and client types.
func NewCombinedResolver(clientConfig *rest.Config) (resolver.SchemaResolver, discovery.DiscoveryInterface, error) {
	// Create a regular discovery client first
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(clientConfig)
	if err != nil {
		return nil, nil, err
	}

	// Wrap it with memory-only caching
	// This caches discovery information in memory with a TTL
	memCachedDiscoveryClient := memory.NewMemCacheClient(discoveryClient)

	// ClientResolver is a resolver that uses the discovery client to resolve
	// client types. It is used to resolve types that are not known at compile
	// time a.k.a present in:
	// https://github.com/kubernetes/apiextensions-apiserver/blob/master/pkg/generated/openapi/zz_generated.openapi.go
	clientResolver := &resolver.ClientDiscoveryResolver{
		Discovery: memCachedDiscoveryClient,
	}

	// CoreResolver is a resolver that uses the OpenAPI definitions to resolve
	// core types. It is used to resolve types that are known at compile time.
	coreResolver := resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)

	// Combine the two resolvers to create a single resolver that can resolve
	// both core and client types.
	combinedResolver := coreResolver.Combine(clientResolver)

	// Wrap with cache to avoid repeated schema resolutions
	// Adjust maxSize based on your expected number of unique GVKs
	cachedResolver := NewTTLCachedSchemaResolver(combinedResolver, 1000, 180*time.Minute)

	return cachedResolver, memCachedDiscoveryClient, nil
}
