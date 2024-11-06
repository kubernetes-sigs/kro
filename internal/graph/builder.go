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

package graph

import (
	"fmt"
	"slices"

	cel "github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"golang.org/x/exp/maps"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/awslabs/symphony/api/v1alpha1"
	scel "github.com/awslabs/symphony/internal/cel"
	"github.com/awslabs/symphony/internal/cel/ast"
	"github.com/awslabs/symphony/internal/graph/crd"
	"github.com/awslabs/symphony/internal/graph/dag"
	"github.com/awslabs/symphony/internal/graph/emulator"
	"github.com/awslabs/symphony/internal/graph/parser"
	"github.com/awslabs/symphony/internal/graph/schema"
	"github.com/awslabs/symphony/internal/graph/variable"
	"github.com/awslabs/symphony/internal/metadata"
	"github.com/awslabs/symphony/internal/simpleschema"
)

// NewBuilder creates a new GraphBuilder instance.
func NewBuilder(
	clientConfig *rest.Config,
) (*Builder, error) {
	schemaResolver, dc, err := schema.NewCombinedResolver(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema resolver: %w", err)
	}

	resourceEmulator := emulator.NewEmulator()

	rgBuilder := &Builder{
		resourceEmulator: resourceEmulator,
		schemaResolver:   schemaResolver,
		discoveryClient:  dc,
	}
	return rgBuilder, nil
}

// Builder is an object that is responsible of constructing and managing
// resourceGroups. It is responsible of transforming the resourceGroup CRD
// into a runtime representation that can be used to create the resources in
// the cluster.
//
// The GraphBuild performs several key functions:
//
//	  1/ It validates the resource deinitions and their naming conventions.
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
// The resulting ResourceGroup object is a fulyl processed and validated
// representation of a resource group CR, it's underlying resources, and the
// relationships between the resources. This object can be used to instantiate
// a "runtime" data structure that can be used to create the resources in the
// cluster.
type Builder struct {
	// schemaResolver is used to resolve the OpenAPI schema for the resources.
	schemaResolver resolver.SchemaResolver
	// resourceEmulator is used to emulate the resources. This is used to validate
	// the CEL expressions in the resources. Because looking up the CEL expressions
	// isn't enough for Symphony to validate the expressions.
	//
	// Maybe there is a better way, if anything probably there is a better way to
	// validate the CEL expressions. To revisit.
	resourceEmulator *emulator.Emulator
	discoveryClient  *discovery.DiscoveryClient
}

// NewResourceGroup creates a new ResourceGroup object from the given ResourceGroup
// CRD. The ResourceGroup object is a fully processed and validated representation
// of the resource group CRD, it's underlying resources, and the relationships between
// the resources.
func (b *Builder) NewResourceGroup(originalCR *v1alpha1.ResourceGroup) (*Graph, error) {
	// Before anything else, let's copy the resource group to avoid modifying the
	// original object.
	rg := originalCR.DeepCopy()

	// There are a few steps to build a resource group:
	// 1. Validate the naming convention of the resource group and its resources.
	//    Symphony leverages CEL expressions to allow users to define new types and
	//    express relationships between resources. This means that we need to ensure
	//    that the names of the resources are valid to be used in CEL expressions.
	//    for example name-something-something is not a valid name for a resource,
	//    because in CEL - is a subtraction operator.
	err := validateResourceGroupNamingConventions(rg)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resourcegroup: %w", err)
	}

	// Now that we did a basic validation of the resource group, we can start understanding
	// the resources that are part of the resource group.

	// For each resource in the resource group, we need to:
	// 1. Check if it looks like a valid Kubernetes resource. This means that it
	//    has a group, version, and kind, and a metadata field.
	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	// 3. Emulate the resource, this is later used to verify the validity of the
	//    CEL expressions.
	// 4. Extract the CEL expressions from the resource + validate them.

	namespacedResources := map[k8sschema.GroupVersionKind]bool{}
	apiResourceList, err := b.discoveryClient.ServerPreferredNamespacedResources()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve Kubernetes namespaced resources: %w", err)
	}
	for _, resourceList := range apiResourceList {
		for _, r := range resourceList.APIResources {
			gvk := k8sschema.FromAPIVersionAndKind(resourceList.GroupVersion, r.Kind)
			namespacedResources[gvk] = r.Namespaced
		}
	}

	// we'll also store the resources in a map for easy access later.
	resources := make(map[string]*Resource)
	for _, rgResource := range rg.Spec.Resources {
		r, err := b.buildRGResource(rgResource, namespacedResources)
		if err != nil {
			return nil, fmt.Errorf("failed to build resource '%v': %v", rgResource.Name, err)
		}
		resources[rgResource.Name] = r
	}

	// At this stage we have a superficial understanding of the resources that are
	// part of the resource group. We have the OpenAPI schema for each resource, and
	// we have extracted the CEL expressions from the schema.
	//
	// Before we get into the dependency graph computation, we need to understand
	// the shape of the instance resource (Mainly trying to understand the instance
	// resource schema) to help validating the CEL expressions that are pointing to
	// the instance resource e.g ${spec.something.something}.
	//
	// You might wonder why are we building the resources before the instance resource?
	// That's because the instance status schema is inferred from the CEL expressions
	// in the status field of the instance resource. Those CEL expressions refer to
	// the resources defined in the resource group. Hence, we need to build the resources
	// first, to be able ot generate a proper schema for the instance status.

	//

	// Next, we need to understand the instance definition. The instance is
	// the resource users will create in their cluster, to request the creation of
	// the resources defined in the resource group.
	//
	// The instance resource is a Kubernetes resource, differently from typical
	// CRDs, users define the schema of the instance resource using the "SimpleSchema"
	// format. This format is a simplified version of the OpenAPI schema, that only
	// supports a subset of the features.
	//
	// SimpleSchema is a new standard we created to simplify CRD declarations, it is
	// very useful when we need to define the Spec of a CRD, when it comes to defining
	// the status of a CRD, we use CEL expressions. Symphony inspects the CEL expressions
	// to infer the types of the status fields, and generate the OpenAPI schema for the
	// status field. The CEL expressions are also used to patch the status field of the
	// instance.
	//
	// We need to:
	// 1. Parse the instance spec fields adhering to the SimpleSchema format.
	// 2. Extract CEL expressions from the status
	// 3. Validate them against the resources defined in the resource group.
	// 4. Infer the status schema based on the CEL expressions.

	instance, err := b.buildInstanceResource(
		rg.Spec.APIVersion,
		rg.Spec.Kind,
		rg.Spec.Definition,
		// We need to pass the resources to the instance resource, so we can validate
		// the CEL expressions in the context of the resources.
		resources,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build resourcegroup %v: %w", err, nil)
	}

	// Before getting into the dependency graph, we need to validate the CEL expressions
	// in the instance resource. In order to do that, we need to isolate each resource
	// and evaluate the CEL expressions in the context of the resource group. This is done
	// by dry-running the CEL expressions against the emulated resources.
	err = validateResourceCELExpressions(resources, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resource CEL expressions: %w", err)
	}

	// Now that we have the instance resource, we can move into the next stage of
	// building the resource group. Understanding the relationships between the
	// resources in the resource group a.k.a the dependency graph.
	//
	// The dependency graph is an directed acyclic graph that represents the
	// relationships between the resources in the resource group. The graph is
	// used to determine the order in which the resources should be created in the
	// cluster.
	//
	// The dependency graph is built by inspecting the CEL expressions in the
	// resources and the instance resource, using a CEL AST (Abstract Syntax Tree)
	// inspector.
	dag, err := b.buildDependencyGraph(resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}

	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to get topological order: %w", err)
	}

	resourceGroup := &Graph{
		DAG:              dag,
		Instance:         instance,
		Resources:        resources,
		TopologicalOrder: topologicalOrder,
	}
	return resourceGroup, nil
}

// buildRGResource builds a resource from the given resource definition.
// It provides a high-level understanding of the resource, by extracting the
// OpenAPI schema, emualting the resource and extracting the cel expressions
// from the schema.
func (b *Builder) buildRGResource(rgResource *v1alpha1.Resource, namespacedResources map[k8sschema.GroupVersionKind]bool) (*Resource, error) {
	// 1. We need to unmashal the resource into a map[string]interface{} to
	//    make it easier to work with.
	resourceObject := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgResource.Definition.Raw, &resourceObject)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.Name, err)
	}

	// 1. Check if it looks like a valid Kubernetes resource.
	err = validateKubernetesObjectStructure(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rgResource.Name, err)
	}

	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	gvk, err := metadata.ExtractGVKFromUnstructured(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rgResource.Name, err)
	}

	// 3. Load the OpenAPI schema for the resource.
	resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema for resource %s: %w", rgResource.Name, err)
	}

	var emulatedResource *unstructured.Unstructured
	var resourceVariables []*variable.ResourceField

	// TODO(michaelhtm): CRDs are not supported for extraction currently
	// implement new logic specific to CRDs
	if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		celExpressions, err := parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			return nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rgResource.Name, err)
		}
		if len(celExpressions) > 0 {
			return nil, fmt.Errorf("failed, CEL expressions are not supported for CRDs, resource %s", rgResource.Name)
		}
	} else {

		// 4. Emulate the resource, this is later used to verify the validity of the
		//    CEL expressions.
		emulatedResource, err = b.resourceEmulator.GenerateDummyCR(gvk, resourceSchema)
		if err != nil {
			return nil, fmt.Errorf("failed to generate dummy CR for resource %s: %w", rgResource.Name, err)
		}

		// 5. Extract CEL fieldDescriptors from the schema.
		fieldDescriptors, err := parser.ParseResource(resourceObject, resourceSchema)
		if err != nil {
			return nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.Name, err)
		}
		for _, fieldDescriptor := range fieldDescriptors {
			resourceVariables = append(resourceVariables, &variable.ResourceField{
				// Assume variables are static, we'll validate them later
				Kind:            variable.ResourceVariableKindStatic,
				FieldDescriptor: fieldDescriptor,
			})
		}
	}

	// 6. Parse ReadyOn expressions
	readyOn, err := parser.ParseConditionExpressions(rgResource.ReadyOn)
	if err != nil {
		return nil, fmt.Errorf("failed to parse readyOn expressions: %v", err)
	}

	conditions, err := parser.ParseConditionExpressions(rgResource.Conditions)
	if err != nil {
		return nil, fmt.Errorf("failed to parse consitional expressions: %v", err)
	}

	_, isNamespaced := namespacedResources[gvk]

	// Note that at this point we don't inject the dependencies into the resource.
	return &Resource{
		id:                   rgResource.Name,
		gvr:                  metadata.GVKtoGVR(gvk),
		schema:               resourceSchema,
		emulatedObject:       emulatedResource,
		originalObject:       &unstructured.Unstructured{Object: resourceObject},
		variables:            resourceVariables,
		readyOnExpressions:   readyOn,
		conditionExpressions: conditions,
		namespaced:           isNamespaced,
	}, nil
}

// buildDependencyGraph builds the dependency graph between the resources in the
// resource group. The dependency graph is an directed acyclic graph that represents
// the relationships between the resources in the resource group. The graph is used
// to determine the order in which the resources should be created in the cluster.
//
// This function returns the DAG, and a map of runtime variables per resource. later
// on we'll use this map to resolve the runtime variables.
func (b *Builder) buildDependencyGraph(
	resources map[string]*Resource,
) (
	// directed acyclic graph
	*dag.DirectedAcyclicGraph,
	// map of runtime variables per resource
	error,
) {

	resourceNames := maps.Keys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "spec")

	env, err := scel.DefaultEnvironment(scel.WithResourceNames(resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	directedAcyclicGraph := dag.NewDirectedAcyclicGraph()
	// Set the vertices of the graph to be the resources defined in the resource group.
	for resourceName := range resources {
		if err := directedAcyclicGraph.AddVertex(resourceName); err != nil {
			return nil, fmt.Errorf("failed to add vertex to graph: %w", err)
		}
	}

	for resourceName, resource := range resources {
		for _, resourceVariable := range resource.variables {
			for _, expression := range resourceVariable.Expressions {
				// We need to inspect the expression to understand how it relates to the
				// resources defined in the resource group.
				err := validateCELExpressionContext(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to validate expression context: %w", err)
				}

				// We need to extract the dependencies from the expression.
				resourceDependencies, isStatic, err := extractDependencies(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies: %w", err)
				}
				if isStatic {
					resourceVariable.Kind = variable.ResourceVariableKindStatic
				} else {
					// If we have seen a dynamic dependency, we need to mark the variable as dynamic.
					resourceVariable.Kind = variable.ResourceVariableKindDynamic
				}

				resource.addDependencies(resourceDependencies...)
				resourceVariable.AddDependencies(resourceDependencies...)
				// We need to add the dependencies to the graph.
				for _, dependency := range resourceDependencies {
					if err := directedAcyclicGraph.AddEdge(resourceName, dependency); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	return directedAcyclicGraph, nil
}

// buildInstanceResource builds the instance resource. The instance resource is
// the representation of the CR that users will create in their cluster to request
// the creation of the resources defined in the resource group.
//
// Since instances are defined using the "SimpleSchema" format, we use a different
// approach to build the instance resource. We need to:
func (b *Builder) buildInstanceResource(
	apiVersion, kind string,
	rgDefinition *v1alpha1.Definition,
	resources map[string]*Resource,
) (*Resource, error) {
	// The instance resource is the resource users will create in their cluster,
	// to request the creation of the resources defined in the resource group.
	//
	// The instance resource is a Kubernetes resource, differently from typical
	// CRDs, it doesn't have an OpenAPI schema. Instead, it has a schema defined
	// using the "SimpleSchema" format, a new standard we created to simplify
	// CRD declarations.

	// The instance resource is a Kubernetes resource, so it has a GroupVersionKind.
	gvk := metadata.GetResourceGroupInstanceGVK(apiVersion, kind)

	// We need to unmarshal the instance definition to a map[string]interface{} to
	// make it easier to work with.
	unstructuredInstance := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgDefinition.Spec.Raw, &unstructuredInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal instance definition: %w", err)
	}

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
	instanceCRD := crd.SynthesizeCRD(apiVersion, kind, *instanceSpecSchema, *instanceStatusSchema, overrideStatusFields)

	// Emulate the CRD
	instanceSchemaExt := instanceCRD.Spec.Versions[0].Schema.OpenAPIV3Schema
	instanceSchema, err := schema.ConvertJSONSchemaPropsToSpecSchema(instanceSchemaExt)
	if err != nil {
		return nil, fmt.Errorf("failed to convert JSON schema to spec schema: %w", err)
	}
	emulatedInstance, err := b.resourceEmulator.GenerateDummyCR(gvk, instanceSchema)
	if err != nil {
		return nil, fmt.Errorf("failed to generate dummy CR for instance: %w", err)
	}

	resourceNames := maps.Keys(resources)
	env, err := scel.DefaultEnvironment(scel.WithResourceNames(resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// The instance resource has a set of variables that need to be resolved.
	instance := &Resource{
		id:             "instance",
		gvr:            metadata.GVKtoGVR(gvk),
		schema:         instanceSchema,
		crd:            instanceCRD,
		emulatedObject: emulatedInstance,
	}

	instanceStatusVariables := []*variable.ResourceField{}
	for _, statusVariable := range statusVariables {
		// These variables needs to be injected into the status field of the instance.
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
func buildInstanceSpecSchema(definition *v1alpha1.Definition) (*extv1.JSONSchemaProps, error) {
	customTypes := map[string]interface{}{}
	err := yaml.UnmarshalStrict(definition.Types.Raw, &customTypes)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal types definition: %w", err)
	}

	// We need to unmarshal the instance definition to a map[string]interface{} to
	// make it easier to work with.
	unstructuredInstance := map[string]interface{}{}
	err = yaml.UnmarshalStrict(definition.Spec.Raw, &unstructuredInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec definition: %w", err)
	}

	// The instance resource has a schema defined using the "SimpleSchema" format.
	instanceSchema, err := simpleschema.NewTransformer().BuildOpenAPISchema(unstructuredInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %v", err)
	}
	return instanceSchema, nil
}

// buildStatusSchema builds the status schema for the instance resource. The
// status schema is inferred from the CEL expressions in the status field.
func buildStatusSchema(
	definition *v1alpha1.Definition,
	resources map[string]*Resource,
) (
	*extv1.JSONSchemaProps,
	[]variable.FieldDescriptor,
	error,
) {
	// The instance resource has a schema defined using the "SimpleSchema" format.
	unstructuredStatus := map[string]interface{}{}
	err := yaml.UnmarshalStrict(definition.Status.Raw, &unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal status definition: %w", err)
	}

	// different from the instance spec, the status schema is inferred from the
	// CEL expressions in the status field.
	fieldDescriptors, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	// Inspection of the CEL expressions to infer the types of the status fields.
	resourceNames := maps.Keys(resources)

	env, err := scel.DefaultEnvironment(scel.WithResourceNames(resourceNames))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// statusStructureParts := make([]schema.FieldDescriptor, 0, len(extracted))
	statusDryRunResults := make(map[string][]ref.Val, len(fieldDescriptors))
	for _, found := range fieldDescriptors {
		// For each expression in the extracted ExpressionField we need to dry-run
		// the expression to infer the type of the status field.
		evals := []ref.Val{}
		for _, expr := range found.Expressions {
			// we need to inspect the expression to understand how it relates to the
			// resources defined in the resource group.
			err := validateCELExpressionContext(env, expr, resourceNames)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to validate expression context: %w", err)
			}

			// resources is the context here.
			value, err := dryRunExpression(env, expr, resources)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to dry-run expression: %w", err)
			}

			evals = append(evals, value)
		}
		statusDryRunResults[found.Path] = evals
	}

	statusSchema, err := schema.GenerateSchemaFromEvals(statusDryRunResults)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build JSON schema from status structure: %w", err)
	}
	return statusSchema, fieldDescriptors, nil
}

// validateCELExpressionContext validates the given CEL expression in the context
// of the resources defined in the resource group.
func validateCELExpressionContext(env *cel.Env, expression string, resources []string) error {
	inspector := ast.NewInspectorWithEnv(env, resources, nil)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource group.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return fmt.Errorf("failed to inspect expression: %w", err)
	}
	// make sure that the expression refers to the resources defined in the resource group.
	for _, resource := range inspectionResult.ResourceDependencies {
		if !slices.Contains(resources, resource.Name) {
			return fmt.Errorf("expression refers to unknown resource: %s", resource.Name)
		}
	}
	return nil
}

// dryRunExpression executes the given CEL expression in the context of a set
// of emulated resources. We could've called this function evaluateExpression
// but we chose to call it dryRunExpression to indicate that we are not actually
// used for anything other than validating the expression and inspecting it
func dryRunExpression(env *cel.Env, expression string, resources map[string]*Resource) (ref.Val, error) {
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to compile expression: %w", issues.Err())
	}

	// TODO(a-hilaly): thinking about a creating a library to hide this...
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create program: %w", err)
	}

	context := map[string]interface{}{}
	for resourceName, resource := range resources {
		context[resourceName] = resource.emulatedObject.Object
	}

	output, _, err := program.Eval(context)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate expression: %w", err)
	}
	return output, nil
}

// extractDependencies extracts the dependencies from the given CEL expression.
// It returns a list of dependencies and a boolea indicating if the expression
// is static or not.
func extractDependencies(env *cel.Env, expression string, resourceNames []string) ([]string, bool, error) {
	// We also want to allow users to refer to the instance spec in their expressions.
	inspector := ast.NewInspectorWithEnv(env, resourceNames, nil)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource group.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect expression: %w", err)
	}

	isStatic := true
	dependencies := make([]string, 0)
	for _, resource := range inspectionResult.ResourceDependencies {
		if resource.Name != "spec" && !slices.Contains(dependencies, resource.Name) {
			isStatic = false
			dependencies = append(dependencies, resource.Name)
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

// validateResourceCELExpressions tries to validate the CEL expressions in the
// resources against the resources defined in the resource group.
//
// In this process, we pin a resource and evaluate the CEL expressions in the
// context of emulated resources. Meaning that given 3 resources A, B, and C,
// we evalute A's CEL expressions against 2 emulated resources B and C. Then
// we evaluate B's CEL expressions against 2 emulated resources A and C, and so
// on.
func validateResourceCELExpressions(resources map[string]*Resource, instance *Resource) error {
	resourceNames := maps.Keys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "spec")
	conditionFieldNames := []string{"spec"}

	env, err := scel.DefaultEnvironment(scel.WithResourceNames(resourceNames))
	if err != nil {
		return fmt.Errorf("failed to create CEL environment: %w", err)
	}

	for _, resource := range resources {
		for _, resourceVariable := range resource.variables {
			for _, expression := range resourceVariable.Expressions {
				err := validateCELExpressionContext(env, expression, resourceNames)
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", expression, err)
				}

				// create context
				context := map[string]*Resource{}
				for resourceName, contextResource := range resources {
					// exclude the resource we are validating
					if resourceName != resource.id {
						context[resourceName] = contextResource
					}
				}
				// add instance spec to the context
				context["spec"] = &Resource{
					emulatedObject: &unstructured.Unstructured{
						Object: instance.emulatedObject.Object["spec"].(map[string]interface{}),
					},
				}

				_, err = dryRunExpression(env, expression, context)
				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", expression, err)
				}
			}
			// validate readyOn Expressions for resource
			// Only accepting expressions accessing the status and spec for now
			// and need to evaluate to a boolean type
			//
			// TODO(michaelhtm) It shares some of the logic with the loop from above..maybe
			// we can refactor them or put it in one function.
			// I would also suggest separating the dryRuns of readyOnExpressions
			// and the resourceExpressions.
			for _, readyOnExpression := range resource.readyOnExpressions {
				fieldNames := schema.GetResourceTopLevelFieldNames(resource.schema)
				fieldEnv, err := scel.DefaultEnvironment(scel.WithResourceNames(fieldNames))
				if err != nil {
					return fmt.Errorf("failed to create CEL environment: %w", err)
				}

				err = validateCELExpressionContext(fieldEnv, readyOnExpression, fieldNames)
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", readyOnExpression, err)
				}
				// create context
				// add resource fields to the context
				context := map[string]*Resource{}
				for _, n := range fieldNames {
					context[n] = &Resource{
						emulatedObject: &unstructured.Unstructured{
							Object: resource.emulatedObject.Object[n].(map[string]interface{}),
						},
					}
				}

				output, err := dryRunExpression(fieldEnv, readyOnExpression, context)

				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", readyOnExpression, err)
				}
				if !scel.IsBoolType(output) {
					return fmt.Errorf("output of readyOn expression %s can only be of type bool", readyOnExpression)
				}
			}

			for _, conditionExpression := range resource.conditionExpressions {
				instanceEnv, err := scel.DefaultEnvironment(scel.WithResourceNames(resourceNames))
				if err != nil {
					return fmt.Errorf("failed to create CEL environment: %w", err)
				}

				err = validateCELExpressionContext(instanceEnv, conditionExpression, conditionFieldNames)
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", conditionExpression, err)
				}
				// create context
				context := map[string]*Resource{}
				// for now we will only support the instance context for condition expressions.
				// With this decision we will decide in creation time, and update time
				// If we'll be creating resources or not
				context["spec"] = &Resource{
					emulatedObject: &unstructured.Unstructured{
						Object: instance.emulatedObject.Object["spec"].(map[string]interface{}),
					},
				}

				output, err := dryRunExpression(instanceEnv, conditionExpression, context)
				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", conditionExpression, err)
				}
				if !scel.IsBoolType(output) {
					return fmt.Errorf("output of condition expression %s can only be of type bool", conditionExpression)
				}
			}
		}
	}

	return nil
}
