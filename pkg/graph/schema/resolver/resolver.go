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
	"errors"
	"net/http"
	"time"

	generatedopenapi "k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ChainedResolver tries a sequence of SchemaResolvers in order, falling
// through on ErrSchemaNotFound.
type ChainedResolver struct {
	resolvers []openapiresolver.SchemaResolver
}

// NewChainedResolver creates a ChainedResolver that tries the given resolvers
// in order. The caller decides the resolution order.
func NewChainedResolver(resolvers ...openapiresolver.SchemaResolver) *ChainedResolver {
	return &ChainedResolver{resolvers: resolvers}
}

// ResolveSchema tries each resolver in order, falling back only on ErrSchemaNotFound.
func (r *ChainedResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	for _, res := range r.resolvers {
		s, err := res.ResolveSchema(gvk)
		if err == nil {
			return s, nil
		}
		if errors.Is(err, openapiresolver.ErrSchemaNotFound) {
			continue
		}
		return nil, err
	}
	return nil, openapiresolver.ErrSchemaNotFound
}

// DefaultCoreResolver creates a resolver for built-in Kubernetes types (Pods,
// Services, Deployments, etc.) using compiled-in OpenAPI definitions.
func DefaultCoreResolver() openapiresolver.SchemaResolver {
	return openapiresolver.NewDefinitionsSchemaResolver(
		generatedopenapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)
}

const (
	// DefaultFallbackMaxSize is the default LRU cache size for the fallback resolver.
	DefaultFallbackMaxSize = 500
	// DefaultFallbackTTL is the default TTL for cached schemas in the fallback resolver.
	DefaultFallbackTTL = 5 * time.Minute
)

// DefaultFallbackResolver creates a TTL-cached discovery resolver that serves
// as a fallback for schemas not resolved by the core or CRD resolvers. This
// covers aggregated API types (e.g. metrics-server, custom API servers) and
// also acts as a warm-up fallback while the CRD informer cache is cold.
func DefaultFallbackResolver(clientConfig *rest.Config, httpClient *http.Client) (*TTLCachedSchemaResolver, error) {
	discoveryClient, err := discovery.NewDiscoveryClientForConfigAndClient(clientConfig, httpClient)
	if err != nil {
		return nil, err
	}
	return NewTTLCachedSchemaResolver(
		&openapiresolver.ClientDiscoveryResolver{Discovery: discoveryClient},
		DefaultFallbackMaxSize,
		DefaultFallbackTTL,
	), nil
}
