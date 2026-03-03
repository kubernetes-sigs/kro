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

package graph

import (
	"fmt"
	"net/http"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	graphparser "github.com/kubernetes-sigs/kro/pkg/graph/parser"
	kmetadata "github.com/kubernetes-sigs/kro/pkg/metadata"

	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
)

// ResolvedNode is a parsed node enriched with API identity and schema metadata.
type ResolvedNode struct {
	ParsedNode
	GVK        schema.GroupVersionKind
	GVR        schema.GroupVersionResource
	Namespaced bool
	Schema     *spec.Schema
}

// ResolvedRGD is the resolver-stage output consumed by the linker.
type ResolvedRGD struct {
	Instance *ParsedInstance
	Nodes    []*ResolvedNode
}

type resolver struct {
	schemas    k8sresolver.SchemaResolver
	restMapper meta.RESTMapper
}

func newResolver(config *rest.Config, httpClient *http.Client) (*resolver, error) {
	sr, err := schemaresolver.NewCombinedResolver(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("schema resolver: %w", err)
	}
	rm, err := apiutil.NewDynamicRESTMapper(config, httpClient)
	if err != nil {
		return nil, fmt.Errorf("REST mapper: %w", err)
	}
	return &resolver{schemas: sr, restMapper: rm}, nil
}

// Resolve takes ParsedRGD, enriches each resource with GVK/GVR/Schema, returns ResolvedRGD.
func (r *resolver) Resolve(parsed *ParsedRGD) (*ResolvedRGD, error) {
	resolved := make([]*ResolvedNode, 0, len(parsed.Nodes))
	for _, pn := range parsed.Nodes {
		rn, err := r.resolveNode(pn)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, rn)
	}
	return &ResolvedRGD{
		Instance: parsed.Instance,
		Nodes:    resolved,
	}, nil
}

func (r *resolver) resolveNode(pr *ParsedNode) (*ResolvedNode, error) {
	gvk, err := kmetadata.ExtractGVKFromUnstructured(pr.Template)
	if err != nil {
		return nil, terminalf("resolver", "resource %q: extract GVK: %v", pr.ID, err)
	}

	resourceSchema, err := r.schemas.ResolveSchema(gvk)
	if err != nil {
		return nil, retriable("resolver", fmt.Errorf("resource %q: schema for %s: %w", pr.ID, gvk, err))
	}

	mapping, err := r.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, retriable("resolver", fmt.Errorf("resource %q: REST mapping for %s: %w", pr.ID, gvk, err))
	}

	namespaced := mapping.Scope.Name() == meta.RESTScopeNameNamespace

	// Scope-dependent validation
	if !namespaced {
		if _, found, _ := unstructured.NestedFieldNoCopy(pr.Template, "metadata", "namespace"); found {
			return nil, terminalf("resolver", "resource %q: cluster-scoped and must not set metadata.namespace", pr.ID)
		}
	}

	// Validate template field types against the resolved schema.
	// This catches mismatches like putting a string where an array is expected.
	if _, err := graphparser.ParseResource(pr.Template, resourceSchema); err != nil {
		return nil, terminalf("resolver", "resource %q: %v", pr.ID, err)
	}

	return &ResolvedNode{
		ParsedNode: *pr,
		GVK:        gvk,
		GVR:        mapping.Resource,
		Namespaced: namespaced,
		Schema:     resourceSchema,
	}, nil
}
