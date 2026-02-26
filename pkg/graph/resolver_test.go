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
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

type resolverSchemaStub struct {
	schema *spec.Schema
	err    error
}

func (s *resolverSchemaStub) ResolveSchema(schema.GroupVersionKind) (*spec.Schema, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.schema, nil
}

type resolverRESTMapperStub struct {
	mapping *meta.RESTMapping
	err     error
}

func (m *resolverRESTMapperStub) KindFor(resource schema.GroupVersionResource) (schema.GroupVersionKind, error) {
	return schema.GroupVersionKind{}, errors.New("not implemented")
}

func (m *resolverRESTMapperStub) KindsFor(resource schema.GroupVersionResource) ([]schema.GroupVersionKind, error) {
	return nil, errors.New("not implemented")
}

func (m *resolverRESTMapperStub) ResourceFor(input schema.GroupVersionResource) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{}, errors.New("not implemented")
}

func (m *resolverRESTMapperStub) ResourcesFor(input schema.GroupVersionResource) ([]schema.GroupVersionResource, error) {
	return nil, errors.New("not implemented")
}

func (m *resolverRESTMapperStub) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.mapping, nil
}

func (m *resolverRESTMapperStub) RESTMappings(gk schema.GroupKind, versions ...string) ([]*meta.RESTMapping, error) {
	if m.err != nil {
		return nil, m.err
	}
	return []*meta.RESTMapping{m.mapping}, nil
}

func (m *resolverRESTMapperStub) ResourceSingularizer(resource string) (string, error) {
	return "", errors.New("not implemented")
}

func TestNewResolver_Cases(t *testing.T) {
	tests := []struct {
		name    string
		config  *rest.Config
		client  *http.Client
		wantErr string
	}{
		{
			name:    "schema resolver creation failure",
			config:  &rest.Config{Host: "http://[::1"},
			client:  &http.Client{},
			wantErr: "schema resolver",
		},
		{
			name:    "rest mapper creation failure",
			config:  &rest.Config{},
			client:  nil,
			wantErr: "REST mapper",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newResolver(tt.config, tt.client)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Nil(t, got)
		})
	}
}

func TestResolver_resolveNode_Cases(t *testing.T) {
	validTemplate := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name": "demo",
		},
	}
	anySchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
		},
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-preserve-unknown-fields": true,
			},
		},
	}

	tests := []struct {
		name    string
		node    *ParsedNode
		r       *resolver
		wantErr string
		term    bool
		retry   bool
		assert  func(*testing.T, *ResolvedNode)
	}{
		{
			name: "extract GVK failure is terminal",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "cfg",
					Template: map[string]interface{}{"invalid": "template"},
				},
			},
			r: &resolver{
				schemas:    &resolverSchemaStub{schema: anySchema},
				restMapper: &resolverRESTMapperStub{},
			},
			wantErr: "extract GVK",
			term:    true,
		},
		{
			name: "schema resolve failure is retriable",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "cfg",
					Template: validTemplate,
				},
			},
			r: &resolver{
				schemas:    &resolverSchemaStub{err: errors.New("schema missing")},
				restMapper: &resolverRESTMapperStub{},
			},
			wantErr: "schema for /v1, Kind=ConfigMap",
			retry:   true,
		},
		{
			name: "REST mapping failure is retriable",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "cfg",
					Template: validTemplate,
				},
			},
			r: &resolver{
				schemas: &resolverSchemaStub{schema: anySchema},
				restMapper: &resolverRESTMapperStub{
					err: errors.New("mapping not found"),
				},
			},
			wantErr: "REST mapping for /v1, Kind=ConfigMap",
			retry:   true,
		},
		{
			name: "success",
			node: &ParsedNode{
				NodeMeta: NodeMeta{
					ID:       "cfg",
					Template: validTemplate,
				},
			},
			r: &resolver{
				schemas: &resolverSchemaStub{schema: anySchema},
				restMapper: &resolverRESTMapperStub{
					mapping: &meta.RESTMapping{
						Resource: schema.GroupVersionResource{Group: "", Version: "v1", Resource: "configmaps"},
						Scope:    meta.RESTScopeNamespace,
					},
				},
			},
			assert: func(t *testing.T, got *ResolvedNode) {
				t.Helper()
				require.NotNil(t, got)
				require.Equal(t, "cfg", got.ID)
				require.True(t, got.Namespaced)
				require.Equal(t, "configmaps", got.GVR.Resource)
				require.NotNil(t, got.Schema)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.r.resolveNode(tt.node)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)
				require.Equal(t, tt.term, IsTerminal(err))
				require.Equal(t, tt.retry, IsRetriable(err))
				return
			}

			require.NoError(t, err)
			if tt.assert != nil {
				tt.assert(t, got)
			}
		})
	}
}
