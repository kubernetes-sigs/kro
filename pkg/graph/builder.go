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
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"golang.org/x/exp/maps"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/graph/crd"
	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// NewBuilder creates a new GraphBuilder instance.
func NewBuilder(clientConfig *rest.Config, httpClient *http.Client) (*Builder, error) {
	schemaResolver, err := schemaresolver.NewCombinedResolver(clientConfig, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema resolver: %w", err)
	}

	rm, err := apiutil.NewDynamicRESTMapper(clientConfig, httpClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic REST mapper: %w", err)
	}

	rgBuilder := &Builder{
		schemaResolver: schemaResolver,
		restMapper:     rm,
	}
	return rgBuilder, nil
}

// Builder is an object that is responsible for constructing and managing
// resourceGraphDefinitions. It is responsible for transforming the resourceGraphDefinition CRD
// into a runtime representation that can be used to create the resources in
// the cluster.
//
// The GraphBuild performs several key functions:
//
//	  1/ It validates the resource definitions and their naming conventions.
//	  2/ It interacts with the API Server to retrieve the OpenAPI schema for the
//	     resources, and validates the resources against the schema.
//	  3/ Extracts and processes the CEL expressions from the resources definitions.
//	  4/ Builds the dependency graph between the resources, by inspecting the CEL
//		    expressions.
//	  5/ It infers and generates the schema for the instance resource, based on the
//			SimpleSchema format.
//
// If any of the above steps fail, the Builder will return an error.
//
// The resulting ResourceGraphDefinition object is a fully processed and validated
// representation of a resource graph definition CR, it's underlying resources, and the
// relationships between the resources. This object can be used to instantiate
// a "runtime" data structure that can be used to create the resources in the
// cluster.
type Builder struct {
	// schemaResolver is used to resolve the OpenAPI schema for the resources.
	schemaResolver resolver.SchemaResolver
	restMapper     meta.RESTMapper
}

// NewResourceGraphDefinition creates a new ResourceGraphDefinition object from the given ResourceGraphDefinition
// CRD. The ResourceGraphDefinition object is a fully processed and validated representation
// of the resource graph definition CRD, it's underlying resources, and the relationships between
// the resources.
func (b *Builder) NewResourceGraphDefinition(originalCR *v1alpha1.ResourceGraphDefinition) (*Graph, error) {
	// Before anything else, let's copy the resource graph definition to avoid modifying the
	// original object.
	rgd := originalCR.DeepCopy()

	// There are a few steps to build a resource graph definition:
	// 1. Validate the naming convention of the resource graph definition and its resources.
	//    kro leverages CEL expressions to allow users to define new types and
	//    express relationships between resources. This means that we need to ensure
	//    that the names of the resources are valid to be used in CEL expressions.
	//    for example name-something-something is not a valid name for a resource,
	//    because in CEL - is a subtraction operator.
	err := validateResourceGraphDefinitionNamingConventions(rgd)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resourcegraphdefinition: %w", err)
	}

	// Now that we did a basic validation of the resource graph definition, we can start understanding
	// the resources that are part of the resource graph definition.

	// For each resource in the resource graph definition, we need to:
	// 1. Check if it looks like a valid Kubernetes resource. This means that it
	//    has a group, version, and kind, and a metadata field.
	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	// 3. Emulate the resource, this is later used to verify the validity of the
	//    CEL expressions.
	// 4. Extract the CEL expressions from the resource + validate them.

	// we'll also store the resources in a map for easy access later.
	resources := make(map[string]*Resource)
	for i, rgResource := range rgd.Spec.Resources {
		id := rgResource.ID
		order := i
		r, err := b.buildRGResource(rgResource, order)
		if err != nil {
			return nil, fmt.Errorf("failed to build resource %q: %w", id, err)
		}
		if resources[id] != nil {
			return nil, fmt.Errorf("found resources with duplicate id %q", id)
		}
		resources[id] = r
	}

	// At this stage we have a superficial understanding of the resources that are
	// part of the resource graph definition. We have the OpenAPI schema for each resource, and
	// we have extracted the CEL expressions from the schema.
	//
	// Before we get into the dependency graph computation, we need to understand
	// the shape of the instance resource (Mainly trying to understand the instance
	// resource schema) to help validating the CEL expressions that are pointing to
	// the instance resource e.g ${schema.spec.something.something}.
	//
	// You might wonder why are we building the resources before the instance resource?
	// That's because the instance status schema is inferred from the CEL expressions
	// in the status field of the instance resource. Those CEL expressions refer to
	// the resources defined in the resource graph definition. Hence, we need to build the resources
	// first, to be able to generate a proper schema for the instance status.

	//

	// Next, we need to understand the instance definition. The instance is
	// the resource users will create in their cluster, to request the creation of
	// the resources defined in the resource graph definition.
	//
	// The instance resource is a Kubernetes resource, differently from typical
	// CRDs, users define the schema of the instance resource using the "SimpleSchema"
	// format. This format is a simplified version of the OpenAPI schema, that only
	// supports a subset of the features.
	//
	// SimpleSchema is a new standard we created to simplify CRD declarations, it is
	// very useful when we need to define the Spec of a CRD, when it comes to defining
	// the status of a CRD, we use CEL expressions. `kro` inspects the CEL expressions
	// to infer the types of the status fields, and generate the OpenAPI schema for the
	// status field. The CEL expressions are also used to patch the status field of the
	// instance.
	//
	// We need to:
	// 1. Parse the instance spec fields adhering to the SimpleSchema format.
	// 2. Extract CEL expressions from the status
	// 3. Validate them against the resources defined in the resource graph definition.
	// 4. Infer the status schema based on the CEL expressions.

	instance, err := b.buildInstanceResource(
		rgd.Spec.Schema.Group,
		rgd.Spec.Schema.APIVersion,
		rgd.Spec.Schema.Kind,
		rgd.Spec.Schema,
		// We need to pass the resources to the instance resource, so we can validate
		// the CEL expressions in the context of the resources.
		resources,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build resourcegraphdefinition '%v': %w", rgd.Name, err)
	}

	// collect all OpenAPI schemas for CEL type checking. This map will be used to
	// create a typed CEL environment that validates expressions against the actual
	// resource schemas.
	schemas := make(map[string]*spec.Schema)
	for id, resource := range resources {
		if resource.schema != nil {
			schemas[id] = resource.schema
		}
	}

	// include the instance spec schema in the context as "schema". This will let us
	// validate expressions such as ${schema.spec.someField}.
	//
	// not that we only include the spec and metadata fields, instance status references
	// are not allowed in RGDs (yet)
	schemaWithoutStatus, err := getSchemaWithoutStatus(instance.crd)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema without status: %w", err)
	}
	schemas["schema"] = schemaWithoutStatus

	// Create a DeclTypeProvider for introspecting type structures during validation
	typeProvider := krocel.CreateDeclTypeProvider(schemas)

	// First, build the dependency graph by inspecting CEL expressions.
	// This extracts all resource dependencies and validates that:
	// 1. All referenced resources are defined in the RGD
	// 2. There are no unknown functions
	// 3. The dependency graph is acyclic
	//
	// We do this BEFORE type checking so that undeclared resource errors
	// are caught here with clear messages, rather than as CEL type errors.
	dag, err := b.buildDependencyGraph(resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}
	// Ensure the graph is acyclic and get the topological order of resources.
	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to get topological order: %w", err)
	}

	// Now that we know all resources are properly declared and dependencies are valid,
	// we can perform type checking on the CEL expressions.

	// Create a typed CEL environment with all resource schemas for template expressions
	templatesEnv, err := krocel.TypedEnvironment(schemas)
	if err != nil {
		return nil, fmt.Errorf("failed to create typed CEL environment: %w", err)
	}

	// Create a CEL environment with only "schema" for includeWhen expressions
	var schemaEnv *cel.Env
	if schemas["schema"] != nil {
		schemaEnv, err = krocel.TypedEnvironment(map[string]*spec.Schema{"schema": schemas["schema"]})
		if err != nil {
			return nil, fmt.Errorf("failed to create CEL environment for includeWhen validation: %w", err)
		}
	}

	// Validate all CEL expressions for each resource node
	for _, resource := range resources {
		if err := validateNode(resource, templatesEnv, schemaEnv, schemas[resource.id], typeProvider); err != nil {
			return nil, fmt.Errorf("failed to validate node %q: %w", resource.id, err)
		}
	}

	resourceGraphDefinition := &Graph{
		DAG:              dag,
		Instance:         instance,
		Resources:        resources,
		TopologicalOrder: topologicalOrder,
	}
	return resourceGraphDefinition, nil
}

// buildExternalRefResource builds an empty resource with metadata from the given externalRef definition.
func (b *Builder) buildExternalRefResource(
	externalRef *v1alpha1.ExternalRef) map[string]interface{} {
	resourceObject := map[string]interface{}{}
	resourceObject["apiVersion"] = externalRef.APIVersion
	resourceObject["kind"] = externalRef.Kind
	metadata := map[string]interface{}{
		"name": externalRef.Metadata.Name,
	}
	if externalRef.Metadata.Namespace != "" {
		metadata["namespace"] = externalRef.Metadata.Namespace
	}
	resourceObject["metadata"] = metadata
	return resourceObject
}

// buildRGResource builds a resource from the given resource definition.
// It provides a high-level understanding of the resource, by extracting the
// OpenAPI schema, emulating the resource and extracting the cel expressions
// from the schema.
func (b *Builder) buildRGResource(
	rgResource *v1alpha1.Resource,
	order int,
) (*Resource, error) {
	// 1. We need to unmarshal the resource into a map[string]interface{} to
	//    make it easier to work with.
	resourceObject := map[string]interface{}{}
	if len(rgResource.Template.Raw) > 0 {
		err := yaml.UnmarshalStrict(rgResource.Template.Raw, &resourceObject)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.ID, err)
		}
	} else if rgResource.ExternalRef != nil {
		resourceObject = b.buildExternalRefResource(rgResource.ExternalRef)
	} else {
		return nil, fmt.Errorf("exactly one of template or externalRef must be provided")
	}

	// 1. Check if it looks like a valid Kubernetes resource.
	err := validateKubernetesObjectStructure(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rgResource.ID, err)
	}

	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	gvk, err := metadata.ExtractGVKFromUnstructured(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rgResource.ID, err)
	}

	// 3. Load the OpenAPI schema for the resource.
	resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema for resource %s: %w", rgResource.ID, err)
	}

	// 5. Extract CEL fieldDescriptors from the resource
	var fieldDescriptors []variable.FieldDescriptor
	if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		fieldDescriptors, err = parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			celExpressionsParseErrorsTotal.WithLabelValues("crd", "parse_failed").Inc()
			return nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rgResource.ID, err)
		}

		celExpressionsParsedTotal.WithLabelValues("crd").Add(float64(len(fieldDescriptors)))

		for _, expr := range fieldDescriptors {
			if !strings.HasPrefix(expr.Path, "metadata.") {
				return nil, fmt.Errorf("CEL expressions in CRDs are only supported for metadata fields, found in path %q, resource %s", expr.Path, rgResource.ID)
			}
		}
	} else {
		fieldDescriptors, err = parser.ParseResource(resourceObject, resourceSchema)
		if err != nil {
			celExpressionsParseErrorsTotal.WithLabelValues("standard", "parse_failed").Inc()
			return nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.ID, err)
		}

		celExpressionsParsedTotal.WithLabelValues("standard").Add(float64(len(fieldDescriptors)))

		// Set ExpectedType on each descriptor by converting schema to CEL type with proper naming
		for i := range fieldDescriptors {
			setExpectedTypeOnDescriptor(&fieldDescriptors[i], resourceSchema, rgResource.ID)
		}
	}

	templateVariables := make([]*variable.ResourceField, 0, len(fieldDescriptors))
	for _, fieldDescriptor := range fieldDescriptors {
		templateVariables = append(templateVariables, &variable.ResourceField{
			// Assume variables are static; we'll validate them later
			Kind:            variable.ResourceVariableKindStatic,
			FieldDescriptor: fieldDescriptor,
		})
	}

	// 6. Parse ReadyWhen expressions
	readyWhen, err := parser.ParseConditionExpressions(rgResource.ReadyWhen)
	if err != nil {
		celExpressionsParseErrorsTotal.WithLabelValues("readyWhen", "parse_failed").Inc()
		return nil, fmt.Errorf("failed to parse readyWhen expressions: %v", err)
	}
	celExpressionsParsedTotal.WithLabelValues("readyWhen").Add(float64(len(readyWhen)))

	// 7. Parse condition expressions
	includeWhen, err := parser.ParseConditionExpressions(rgResource.IncludeWhen)
	if err != nil {
		celExpressionsParseErrorsTotal.WithLabelValues("includeWhen", "parse_failed").Inc()
		return nil, fmt.Errorf("failed to parse includeWhen expressions: %v", err)
	}
	celExpressionsParsedTotal.WithLabelValues("includeWhen").Add(float64(len(includeWhen)))

	mapping, err := b.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("failed to get REST mapping for resource %s: %w", rgResource.ID, err)
	}

	// Note that at this point we don't inject the dependencies into the resource.
	return &Resource{
		id:                     rgResource.ID,
		gvr:                    mapping.Resource,
		schema:                 resourceSchema,
		originalObject:         &unstructured.Unstructured{Object: resourceObject},
		variables:              templateVariables,
		readyWhenExpressions:   readyWhen,
		includeWhenExpressions: includeWhen,
		namespaced:             mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		order:                  order,
		isExternalRef:          rgResource.ExternalRef != nil,
	}, nil
}

// buildDependencyGraph builds the dependency graph between the resources in the
// resource graph definition.
// The dependency graph is a directed acyclic graph that represents
// the relationships between the resources in the resource graph definition.
// The graph is used
// to determine the order in which the resources should be created in the cluster.
//
// This function returns the DAG, and a map of runtime variables per resource.
// Later
//
//	on, we'll use this map to resolve the runtime variables.
func (b *Builder) buildDependencyGraph(
	resources map[string]*Resource,
) (
	*dag.DirectedAcyclicGraph[string], // directed acyclic graph
	error,
) {

	resourceNames := maps.Keys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "schema")

	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	directedAcyclicGraph := dag.NewDirectedAcyclicGraph[string]()
	// Set the vertices of the graph to be the resources defined in the resource graph definition.
	for _, resource := range resources {
		if err := directedAcyclicGraph.AddVertex(resource.id, resource.order); err != nil {
			return nil, fmt.Errorf("failed to add vertex to graph: %w", err)
		}
	}

	for _, resource := range resources {
		for _, templateVariable := range resource.variables {
			for _, expression := range templateVariable.Expressions {
				// We need to extract the dependencies from the expression.
				resourceDependencies, isStatic, err := extractDependencies(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies: %w", err)
				}

				// Static until proven dynamic.
				//
				// This reads as: If the expression is dynamic and the template variable is
				// static, then we need to mark the template variable as dynamic.
				if !isStatic && templateVariable.Kind == variable.ResourceVariableKindStatic {
					templateVariable.Kind = variable.ResourceVariableKindDynamic
				}

				resource.addDependencies(resourceDependencies...)
				templateVariable.AddDependencies(resourceDependencies...)
				// We need to add the dependencies to the graph.
				if err := directedAcyclicGraph.AddDependencies(resource.id, resourceDependencies); err != nil {
					return nil, err
				}
			}
		}
	}

	return directedAcyclicGraph, nil
}

// buildInstanceResource builds the instance resource. The instance resource is
// the representation of the CR that users will create in their cluster to request
// the creation of the resources defined in the resource graph definition.
//
// Since instances are defined using the "SimpleSchema" format, we use a different
// approach to build the instance resource. We need to:
func (b *Builder) buildInstanceResource(
	group, apiVersion, kind string,
	rgDefinition *v1alpha1.Schema,
	resources map[string]*Resource,
) (*Resource, error) {
	// The instance resource is the resource users will create in their cluster,
	// to request the creation of the resources defined in the resource graph definition.
	//
	// The instance resource is a Kubernetes resource, differently from typical
	// CRDs; it doesn't have an OpenAPI schema. Instead, it has a schema defined
	// using the "SimpleSchema" format, a new standard we created to simplify
	// CRD declarations.

	// The instance resource is a Kubernetes resource, so it has a GroupVersionKind.
	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(group, apiVersion, kind)

	// The instance resource has a schema defined using the "SimpleSchema" format.
	instanceSpecSchema, err := buildInstanceSpecSchema(rgDefinition)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %w", err)
	}

	instanceStatusSchema, statusVariables, err := buildStatusSchema(rgDefinition, resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance status: %w", err)
	}

	// Synthesize the CRD for the instance resource.
	overrideStatusFields := true
	instanceCRD := crd.SynthesizeCRD(group, apiVersion, kind, *instanceSpecSchema, *instanceStatusSchema, overrideStatusFields, rgDefinition.AdditionalPrinterColumns)

	instanceSchemaExt := instanceCRD.Spec.Versions[0].Schema.OpenAPIV3Schema
	instanceSchema, err := schema.ConvertJSONSchemaPropsToSpecSchema(instanceSchemaExt)
	if err != nil {
		return nil, fmt.Errorf("failed to convert JSON schema to spec schema: %w", err)
	}

	resourceNames := maps.Keys(resources)
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// The instance resource has a set of variables that need to be resolved.
	instance := &Resource{
		id:     "instance",
		gvr:    gvr,
		schema: instanceSchema,
		crd:    instanceCRD,
	}

	instanceStatusVariables := []*variable.ResourceField{}
	for _, statusVariable := range statusVariables {
		// These variables need to be injected into the status field of the instance.
		path := "status." + statusVariable.Path
		statusVariable.Path = path

		instanceDependencies, isStatic, err := extractDependencies(env, statusVariable.Expressions[0], resourceNames)
		if err != nil {
			return nil, fmt.Errorf("failed to extract dependencies: %w", err)
		}
		if isStatic {
			return nil, fmt.Errorf("instance status field must refer to a resource: %s", statusVariable.Path)
		}
		instance.addDependencies(instanceDependencies...)

		instanceStatusVariables = append(instanceStatusVariables, &variable.ResourceField{
			FieldDescriptor: statusVariable,
			Kind:            variable.ResourceVariableKindDynamic,
			Dependencies:    instanceDependencies,
		})
	}

	instance.variables = instanceStatusVariables
	return instance, nil
}

// buildInstanceSpecSchema builds the instance spec schema that will be
// used to generate the CRD for the instance resource. The instance spec
// schema is expected to be defined using the "SimpleSchema" format.
func buildInstanceSpecSchema(rgSchema *v1alpha1.Schema) (*extv1.JSONSchemaProps, error) {
	// We need to unmarshal the instance schema to a map[string]interface{} to
	// make it easier to work with.
	instanceSpec := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgSchema.Spec.Raw, &instanceSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec schema: %w", err)
	}

	// Also the custom types must be unmarshalled to a map[string]interface{} to
	// make handling easier.
	customTypes := map[string]interface{}{}
	err = yaml.UnmarshalStrict(rgSchema.Types.Raw, &customTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal predefined types: %w", err)
	}

	// The instance resource has a schema defined using the "SimpleSchema" format.
	instanceSchema, err := simpleschema.ToOpenAPISpec(instanceSpec, customTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %v", err)
	}

	return instanceSchema, nil
}

// buildStatusSchema builds the status schema for the instance resource.
// The status schema is inferred from the CEL expressions in the status field
// using CEL type checking.
func buildStatusSchema(
	rgSchema *v1alpha1.Schema,
	resources map[string]*Resource,
) (
	*extv1.JSONSchemaProps,
	[]variable.FieldDescriptor,
	error,
) {
	// The instance resource has a schema defined using the "SimpleSchema" format.
	unstructuredStatus := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgSchema.Status.Raw, &unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal status schema: %w", err)
	}

	// Extract CEL expressions from the status field.
	fieldDescriptors, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		celExpressionsParseErrorsTotal.WithLabelValues("instance_status", "parse_failed").Inc()
		return nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	celExpressionsParsedTotal.WithLabelValues("instance_status").Add(float64(len(fieldDescriptors)))

	schemas := make(map[string]*spec.Schema)
	for id, resource := range resources {
		if resource.schema != nil {
			schemas[id] = resource.schema
		}
	}

	env, err := krocel.TypedEnvironment(schemas)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create typed CEL environment: %w", err)
	}

	provider := krocel.CreateDeclTypeProvider(schemas)

	// Infer types for each status field expression using CEL type checking
	statusTypeMap := make(map[string]*cel.Type)
	for _, fieldDescriptor := range fieldDescriptors {
		if len(fieldDescriptor.Expressions) == 1 {
			// Single expression - must infer type from expression output
			expression := fieldDescriptor.Expressions[0]

			checkedAST, err := parseAndCheckCELExpression(env, expression)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to type-check status expression %q at path %q: %w", expression, fieldDescriptor.Path, err)
			}

			statusTypeMap[fieldDescriptor.Path] = checkedAST.OutputType()
		} else {
			for _, expression := range fieldDescriptor.Expressions {
				checkedAST, err := parseAndCheckCELExpression(env, expression)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to type-check status expression %q at path %q: %w", expression, fieldDescriptor.Path, err)
				}

				outputType := checkedAST.OutputType()
				if err := validateExpressionType(outputType, cel.StringType, expression, "status", fieldDescriptor.Path, provider); err != nil {
					return nil, nil, err
				}
			}
			// All expressions are strings - result type is string
			statusTypeMap[fieldDescriptor.Path] = cel.StringType
		}
	}

	// convert the CEL types to OpenAPI schema - best effort.
	statusSchema, err := schema.GenerateSchemaFromCELTypes(statusTypeMap, provider)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate status schema from CEL types: %w", err)
	}

	return statusSchema, fieldDescriptors, nil
}

// extractDependencies extracts the dependencies from the given CEL expression.
// It returns a list of dependencies and a boolean indicating if the expression
// is static or not.
func extractDependencies(env *cel.Env, expression string, resourceNames []string) ([]string, bool, error) {
	// We also want to allow users to refer to the instance spec in their expressions.
	inspector := ast.NewInspectorWithEnv(env, resourceNames)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource graph definition.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect expression: %w", err)
	}

	isStatic := true
	dependencies := make([]string, 0)
	for _, resource := range inspectionResult.ResourceDependencies {
		if resource.ID != "schema" && !slices.Contains(dependencies, resource.ID) {
			isStatic = false
			dependencies = append(dependencies, resource.ID)
		}
	}
	if len(inspectionResult.UnknownResources) > 0 {
		return nil, false, fmt.Errorf("found unknown resources in CEL expression: [%v]", inspectionResult.UnknownResources)
	}
	if len(inspectionResult.UnknownFunctions) > 0 {
		return nil, false, fmt.Errorf("found unknown functions in CEL expression: [%v]", inspectionResult.UnknownFunctions)
	}
	return dependencies, isStatic, nil
}

// setExpectedTypeOnDescriptor sets the ExpectedType field on a FieldDescriptor.
// This is the single place where ExpectedType is determined for all field descriptors.
//
// For string templates (multiple expressions like "foo-${expr1}-${expr2}"):
//   - Always sets to cel.StringType since concatenation produces strings
//
// For standalone expressions (single expression like "${expr}"):
//  1. Parses path into segments (e.g., "spec.containers[0].name" -> ["spec", "containers", [0], "name"])
//  2. Walks through each segment, building type name and navigating schema:
//     - Named segments: append to type name, look up in schema
//     - Index segments: dereference array to element schema, append ".@idx" to type name
//  3. Converts final schema to CEL type with constructed type name
func resolveSchemaAndTypeName(segments []fieldpath.Segment, rootSchema *spec.Schema, resourceID string) (*spec.Schema, string, error) {
	typeName := krocel.TypeNamePrefix + resourceID
	currentSchema := rootSchema

	for _, seg := range segments {
		if seg.Name != "" {
			typeName = typeName + "." + seg.Name
			currentSchema = lookupSchemaAtPath(currentSchema, seg.Name)
			if currentSchema == nil {
				return nil, "", fmt.Errorf("field %q not found in schema", seg.Name)
			}
		}

		if seg.Index != -1 {
			if currentSchema.Items != nil && currentSchema.Items.Schema != nil {
				currentSchema = currentSchema.Items.Schema
				typeName = typeName + ".@idx"
			} else {
				return nil, "", fmt.Errorf("field is not an array")
			}
		}
	}

	return currentSchema, typeName, nil
}

func setExpectedTypeOnDescriptor(descriptor *variable.FieldDescriptor, rootSchema *spec.Schema, resourceID string) {
	if !descriptor.StandaloneExpression {
		descriptor.ExpectedType = cel.StringType
		return
	}

	segments, err := fieldpath.Parse(descriptor.Path)
	if err != nil {
		descriptor.ExpectedType = cel.DynType
		return
	}

	schema, typeName, err := resolveSchemaAndTypeName(segments, rootSchema, resourceID)
	if err != nil {
		descriptor.ExpectedType = cel.DynType
		return
	}

	descriptor.ExpectedType = getCelTypeFromSchema(schema, typeName)
}

// getCelTypeFromSchema converts an OpenAPI schema to a CEL type with the given type name
func getCelTypeFromSchema(schema *spec.Schema, typeName string) *cel.Type {
	if schema == nil {
		return cel.DynType
	}

	declType := krocel.SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: schema}, false)
	if declType == nil {
		return cel.DynType
	}

	declType = declType.MaybeAssignTypeName(typeName)
	return declType.CelType()
}

// lookupSchemaAtPath traverses a schema following a field path and returns the schema at that location
func lookupSchemaAtPath(schema *spec.Schema, path string) *spec.Schema {
	if path == "" {
		return schema
	}

	// Split path by "." to get field names
	parts := strings.Split(path, ".")
	current := schema

	for _, part := range parts {
		if current == nil {
			return nil
		}

		// Check if it's an object with properties
		if prop, ok := current.Properties[part]; ok {
			current = &prop
			continue
		}

		// Check if it's an array and we need to look at items
		if current.Items != nil && current.Items.Schema != nil {
			current = current.Items.Schema
			// Try again with this part on the items schema
			if prop, ok := current.Properties[part]; ok {
				current = &prop
				continue
			}
		}

		// Couldn't find the field
		return nil
	}

	return current
}

// validateNode validates all CEL expressions for a single resource node:
// - Template expressions (resource field values)
// - includeWhen expressions (conditional resource creation)
// - readyWhen expressions (resource readiness conditions)
func validateNode(resource *Resource, templatesEnv, schemaEnv *cel.Env, resourceSchema *spec.Schema, typeProvider *krocel.DeclTypeProvider) error {
	// Validate template expressions
	if err := validateTemplateExpressions(templatesEnv, resource, typeProvider); err != nil {
		return err
	}

	// Validate includeWhen expressions if present
	if len(resource.includeWhenExpressions) > 0 {
		if err := validateIncludeWhenExpressions(schemaEnv, resource); err != nil {
			return err
		}
	}

	// Validate readyWhen expressions if present
	if len(resource.readyWhenExpressions) > 0 {
		// Create a CEL environment with only this resource's schema available
		resourceEnv, err := krocel.TypedEnvironment(map[string]*spec.Schema{resource.id: resourceSchema})
		if err != nil {
			return fmt.Errorf("failed to create CEL environment for readyWhen validation: %w", err)
		}

		if err := validateReadyWhenExpressions(resourceEnv, resource); err != nil {
			return err
		}
	}

	return nil
}

// validateTemplateExpressions validates CEL template expressions for a single resource.
// It type-checks that expressions reference valid fields and return the expected types
// based on the OpenAPI schemas.
func validateTemplateExpressions(env *cel.Env, resource *Resource, typeProvider *krocel.DeclTypeProvider) error {
	for _, templateVariable := range resource.variables {
		if len(templateVariable.Expressions) == 1 {
			// Single expression - validate against expected types
			expression := templateVariable.Expressions[0]

			checkedAST, err := parseAndCheckCELExpression(env, expression)
			if err != nil {
				return fmt.Errorf("failed to type-check template expression %q at path %q: %w", expression, templateVariable.Path, err)
			}
			outputType := checkedAST.OutputType()
			if err := validateExpressionType(outputType, templateVariable.ExpectedType, expression, resource.id, templateVariable.Path, typeProvider); err != nil {
				return err
			}
		} else if len(templateVariable.Expressions) > 1 {
			// Multiple expressions - all must be strings for concatenation
			for _, expression := range templateVariable.Expressions {
				checkedAST, err := parseAndCheckCELExpression(env, expression)
				if err != nil {
					return fmt.Errorf("failed to type-check template expression %q at path %q: %w", expression, templateVariable.Path, err)
				}

				outputType := checkedAST.OutputType()
				if err := validateExpressionType(outputType, templateVariable.ExpectedType, expression, resource.id, templateVariable.Path, typeProvider); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateExpressionType verifies that the CEL expression output type matches
// the expected type. Returns an error if there is a type mismatch.
func validateExpressionType(outputType, expectedType *cel.Type, expression, resourceID, path string, typeProvider *krocel.DeclTypeProvider) error {
	// Try CEL's built-in nominal type checking first
	if expectedType.IsAssignableType(outputType) {
		return nil
	}

	// Try structural compatibility checking (duck typing)
	compatible, compatErr := krocel.AreTypesStructurallyCompatible(outputType, expectedType, typeProvider)
	if compatible {
		return nil
	}
	// If we have a detailed compatibility error, use it
	if compatErr != nil {
		return fmt.Errorf(
			"type mismatch in resource %q at path %q: expression %q returns type %q but expected %q: %w",
			resourceID, path, expression, outputType.String(), expectedType.String(), compatErr,
		)
	}

	// Type mismatch - construct helpful error message. This will surface to users.
	return fmt.Errorf(
		"type mismatch in resource %q at path %q: expression %q returns type %q but expected %q",
		resourceID, path, expression, outputType.String(), expectedType.String(),
	)
}

// parseAndCheckCELExpression parses and type-checks a CEL expression.
// Returns the checked AST on success, or the raw CEL error on failure.
// Callers should wrap the error with appropriate context.
func parseAndCheckCELExpression(env *cel.Env, expression string) (*cel.Ast, error) {
	parsedAST, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	checkedAST, issues := env.Check(parsedAST)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	return checkedAST, nil
}

// validateConditionExpression validates a single condition expression (includeWhen or readyWhen).
// It parses, type-checks, and verifies the expression returns bool or optional_type(bool).
func validateConditionExpression(env *cel.Env, expression, conditionType, resourceID string) error {
	checkedAST, err := parseAndCheckCELExpression(env, expression)
	if err != nil {
		return fmt.Errorf("failed to type-check %s expression %q in resource %q: %w", conditionType, expression, resourceID, err)
	}

	// Verify the expression returns bool or optional_type(bool)
	outputType := checkedAST.OutputType()
	if !krocel.IsBoolOrOptionalBool(outputType) {
		return fmt.Errorf(
			"%s expression %q in resource %q must return bool or optional_type(bool), but returns %q",
			conditionType, expression, resourceID, outputType.String(),
		)
	}

	return nil
}

// validateIncludeWhenExpressions validates that includeWhen expressions:
// 1. Only reference the "schema" variable
// 2. Return bool or optional_type(bool)
// validateIncludeWhenExpressions validates includeWhen expressions for a single resource.
func validateIncludeWhenExpressions(env *cel.Env, resource *Resource) error {
	for _, expression := range resource.includeWhenExpressions {
		if err := validateConditionExpression(env, expression, "includeWhen", resource.id); err != nil {
			return err
		}
	}
	return nil
}

// validateReadyWhenExpressions validates readyWhen expressions for a single resource.
func validateReadyWhenExpressions(env *cel.Env, resource *Resource) error {
	for _, expression := range resource.readyWhenExpressions {
		if err := validateConditionExpression(env, expression, "readyWhen", resource.id); err != nil {
			return err
		}
	}
	return nil
}

// getSchemaWithoutStatus returns a schema from the CRD with the status field removed.
func getSchemaWithoutStatus(crd *extv1.CustomResourceDefinition) (*spec.Schema, error) {
	crdCopy := crd.DeepCopy()

	// TODO(a-hilaly) expand this function when we start support CRD upgrades.
	if len(crdCopy.Spec.Versions) != 1 {
		return nil, fmt.Errorf("expected CRD to have exactly one version, got %d versions: multi-version CRDs not yet supported", len(crdCopy.Spec.Versions))
	}
	if crdCopy.Spec.Versions[0].Schema == nil {
		return nil, fmt.Errorf("expected CRD version to have schema defined, but schema is nil")
	}

	openAPISchema := crdCopy.Spec.Versions[0].Schema.OpenAPIV3Schema

	if openAPISchema.Properties == nil {
		openAPISchema.Properties = make(map[string]extv1.JSONSchemaProps)
	}

	delete(openAPISchema.Properties, "status")

	specSchema, err := schema.ConvertJSONSchemaPropsToSpecSchema(openAPISchema)
	if err != nil {
		return nil, err
	}

	if specSchema.Properties == nil {
		specSchema.Properties = make(map[string]spec.Schema)
	}
	specSchema.Properties["metadata"] = schema.ObjectMetaSchema
	return specSchema, nil
}
