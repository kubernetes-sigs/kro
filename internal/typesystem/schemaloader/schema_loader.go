// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package schemaloader

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
)

type Schema struct {
	OpenAPIV3Schema *apiextensionsv1.JSONSchemaProps
}

type SchemaLoader struct {
	clientset              *kubernetes.Clientset
	apiextensionsClientset *apiextensionsclientset.Clientset
	mapper                 *restmapper.DeferredDiscoveryRESTMapper
	discoveryClient        discovery.DiscoveryInterface
	definitions            map[string]apiextensionsv1.JSONSchemaProps
}

func NewSchemaLoader(clientset *kubernetes.Clientset, apiextensionsClientset *apiextensionsclientset.Clientset) *SchemaLoader {
	dc := memory.NewMemCacheClient(clientset.Discovery())
	// Use dc.Invalidate() to refresh the cache

	mapper := restmapper.NewDeferredDiscoveryRESTMapper(dc)

	return &SchemaLoader{
		clientset:              clientset,
		apiextensionsClientset: apiextensionsClientset,
		mapper:                 mapper,
		discoveryClient:        clientset.Discovery(),
		definitions:            make(map[string]apiextensionsv1.JSONSchemaProps),
	}
}

func (s *SchemaLoader) GetSchema(gvk schema.GroupVersionKind) (*Schema, error) {
	// First, try to get the schema as a CRD
	crd, err := s.getCRD(gvk)
	if err == nil {
		return s.schemaFromCRD(crd), nil
	}

	// If it's not a CRD, try to get it from the OpenAPI spec
	return s.schemaFromOpenAPI(gvk)
}

func (s *SchemaLoader) getCRD(gvk schema.GroupVersionKind) (*apiextensionsv1.CustomResourceDefinition, error) {
	crdName := fmt.Sprintf("%s.%s", strings.ToLower(gvk.Kind), gvk.Group)
	return s.apiextensionsClientset.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), crdName, metav1.GetOptions{})
}

func (s *SchemaLoader) schemaFromCRD(crd *apiextensionsv1.CustomResourceDefinition) *Schema {
	for _, version := range crd.Spec.Versions {
		if version.Served && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			return &Schema{
				OpenAPIV3Schema: version.Schema.OpenAPIV3Schema,
			}
		}
	}
	return nil
}

func (s *SchemaLoader) schemaFromOpenAPI(gvk schema.GroupVersionKind) (*Schema, error) {
	data, err := s.discoveryClient.RESTClient().Get().AbsPath("/openapi/v2").Do(context.TODO()).Raw()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OpenAPI schema: %v", err)
	}

	var openAPISchema map[string]interface{}
	if err := json.Unmarshal(data, &openAPISchema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal OpenAPI schema: %v", err)
	}

	definitions, ok := openAPISchema["definitions"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("failed to find definitions in OpenAPI schema")
	}

	s.definitions = make(map[string]apiextensionsv1.JSONSchemaProps)
	for definitionName, schemaData := range definitions {
		jsonSchema, err := convertToJSONSchemaProps(schemaData)
		if err != nil {
			return nil, fmt.Errorf("failed to convert definition %s: %v", definitionName, err)
		}
		s.definitions[definitionName] = *jsonSchema
	}

	gvkString := getOpenAPIDefinitionName(gvk)
	schemaProps, ok := s.definitions[gvkString]
	if !ok {
		return nil, fmt.Errorf("schema not found for %v", gvk)
	}

	resolvedSchema, err := s.resolveReferences(&schemaProps)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve references: %v", err)
	}

	return &Schema{OpenAPIV3Schema: resolvedSchema}, nil
}

// very wrong i think?? maybe not
func convertToJSONSchemaProps(data interface{}) (*apiextensionsv1.JSONSchemaProps, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal schema data: %v", err)
	}

	var jsonSchema apiextensionsv1.JSONSchemaProps
	if err := json.Unmarshal(jsonData, &jsonSchema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal schema data: %v", err)
	}

	return &jsonSchema, nil
}

func (s *SchemaLoader) resolveReferences(schema *apiextensionsv1.JSONSchemaProps) (*apiextensionsv1.JSONSchemaProps, error) {
	if schema == nil {
		return nil, nil
	}

	if schema.Ref != nil {
		refParts := strings.Split(*schema.Ref, "/")
		if len(refParts) > 0 {
			refName := refParts[len(refParts)-1]
			if refSchema, ok := s.definitions[refName]; ok {
				return s.resolveReferences(&refSchema)
			}
		}
		return nil, fmt.Errorf("failed to resolve reference: %s", *schema.Ref)
	}

	for k, v := range schema.Properties {
		resolved, err := s.resolveReferences(&v)
		if err != nil {
			return nil, err
		}
		schema.Properties[k] = *resolved
	}

	if schema.Items != nil && schema.Items.Schema != nil {
		resolved, err := s.resolveReferences(schema.Items.Schema)
		if err != nil {
			return nil, err
		}
		schema.Items.Schema = resolved
	}

	return schema, nil
}

func getOpenAPIDefinitionName(gvk schema.GroupVersionKind) string {
	if gvk.Group == "" {
		return fmt.Sprintf("io.k8s.api.core.%s.%s", gvk.Version, gvk.Kind)
	}
	return fmt.Sprintf("io.k8s.api.%s.%s.%s", gvk.Group, gvk.Version, gvk.Kind)
}
