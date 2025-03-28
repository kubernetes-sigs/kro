// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package graph

import (
	"fmt"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	"golang.org/x/exp/maps"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/kro-run/kro/api/v1alpha1"
	krocel "github.com/kro-run/kro/pkg/cel"
	"github.com/kro-run/kro/pkg/cel/ast"
	"github.com/kro-run/kro/pkg/graph/crd"
	"github.com/kro-run/kro/pkg/graph/dag"
	"github.com/kro-run/kro/pkg/graph/emulator"
	"github.com/kro-run/kro/pkg/graph/parser"
	"github.com/kro-run/kro/pkg/graph/schema"
	"github.com/kro-run/kro/pkg/graph/variable"
	"github.com/kro-run/kro/pkg/metadata"
	"github.com/kro-run/kro/pkg/simpleschema"
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
// resourceGraphDefinitions. It is responsible of transforming the resourceGraphDefinition CRD
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
// The resulting ResourceGraphDefinition object is a fulyl processed and validated
// representation of a resource graph definition CR, it's underlying resources, and the
// relationships between the resources. This object can be used to instantiate
// a "runtime" data structure that can be used to create the resources in the
// cluster.
type Builder struct {
	// schemaResolver is used to resolve the OpenAPI schema for the resources.
	schemaResolver resolver.SchemaResolver
	// resourceEmulator is used to emulate the resources. This is used to validate
	// the CEL expressions in the resources. Because looking up the CEL expressions
	// isn't enough for kro to validate the expressions.
	//
	// Maybe there is a better way, if anything probably there is a better way to
	// validate the CEL expressions. To revisit.
	resourceEmulator *emulator.Emulator
	discoveryClient  discovery.DiscoveryInterface
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

	namespacedResources := map[k8sschema.GroupKind]bool{}
	apiResourceList, err := b.discoveryClient.ServerPreferredNamespacedResources()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve Kubernetes namespaced resources: %w", err)
	}
	for _, resourceList := range apiResourceList {
		for _, r := range resourceList.APIResources {
			gvk := k8sschema.FromAPIVersionAndKind(resourceList.GroupVersion, r.Kind)
			namespacedResources[gvk.GroupKind()] = r.Namespaced
		}
	}

	// we'll also store the resources in a map for easy access later.
	resources := make(map[string]*Resource)
	for i, rgResource := range rgd.Spec.Resources {
		id := rgResource.ID
		order := i
		r, err := b.buildRGResource(rgResource, namespacedResources, order)
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

	// Before getting into the dependency graph, we need to validate the CEL expressions
	// in the instance resource. In order to do that, we need to isolate each resource
	// and evaluate the CEL expressions in the context of the resource graph definition. This is done
	// by dry-running the CEL expressions against the emulated resources.
	err = validateResourceCELExpressions(resources, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resource CEL expressions: %w", err)
	}

	// Now that we have the instance resource, we can move into the next stage of
	// building the resource graph definition. Understanding the relationships between the
	// resources in the resource graph definition a.k.a the dependency graph.
	//
	// The dependency graph is an directed acyclic graph that represents the
	// relationships between the resources in the resource graph definition. The graph is
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

	resourceGraphDefinition := &Graph{
		DAG:              dag,
		Instance:         instance,
		Resources:        resources,
		TopologicalOrder: topologicalOrder,
	}
	return resourceGraphDefinition, nil
}

// buildRGResource builds a resource from the given resource definition.
// It provides a high-level understanding of the resource, by extracting the
// OpenAPI schema, emulating the resource and extracting the cel expressions
// from the schema.
func (b *Builder) buildRGResource(rgResource *v1alpha1.Resource, namespacedResources map[k8sschema.GroupKind]bool, order int) (*Resource, error) {
	// 1. We need to unmarshal the resource into a map[string]interface{} to
	//    make it easier to work with.
	resourceObject := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgResource.Template.Raw, &resourceObject)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.ID, err)
	}

	// 1. Check if it looks like a valid Kubernetes resource.
	err = validateKubernetesObjectStructure(resourceObject)
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

	var emulatedResource *unstructured.Unstructured
	var resourceVariables []*variable.ResourceField

	// TODO(michaelhtm): CRDs are not supported for extraction currently
	// implement new logic specific to CRDs
	if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		celExpressions, err := parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			return nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rgResource.ID, err)
		}
		if len(celExpressions) > 0 {
			return nil, fmt.Errorf("failed, CEL expressions are not supported for CRDs, resource %s", rgResource.ID)
		}
	} else {

		// 4. Emulate the resource, this is later used to verify the validity of the
		//    CEL expressions.
		emulatedResource, err = b.resourceEmulator.GenerateDummyCR(gvk, resourceSchema)
		if err != nil {
			return nil, fmt.Errorf("failed to generate dummy CR for resource %s: %w", rgResource.ID, err)
		}

		// 5. Extract CEL fieldDescriptors from the schema.
		fieldDescriptors, err := parser.ParseResource(resourceObject, resourceSchema)
		if err != nil {
			return nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.ID, err)
		}
		for _, fieldDescriptor := range fieldDescriptors {
			resourceVariables = append(resourceVariables, &variable.ResourceField{
				// Assume variables are static, we'll validate them later
				Kind:            variable.ResourceVariableKindStatic,
				FieldDescriptor: fieldDescriptor,
			})
		}
	}

	// 6. Parse ReadyWhen expressions
	readyWhen, err := parser.ParseConditionExpressions(rgResource.ReadyWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse readyWhen expressions: %v", err)
	}

	// 7. Parse condition expressions
	includeWhen, err := parser.ParseConditionExpressions(rgResource.IncludeWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse includeWhen expressions: %v", err)
	}

	_, isNamespaced := namespacedResources[gvk.GroupKind()]

	// Note that at this point we don't inject the dependencies into the resource.
	return &Resource{
		id:                     rgResource.ID,
		gvr:                    metadata.GVKtoGVR(gvk),
		schema:                 resourceSchema,
		emulatedObject:         emulatedResource,
		originalObject:         &unstructured.Unstructured{Object: resourceObject},
		variables:              resourceVariables,
		readyWhenExpressions:   readyWhen,
		includeWhenExpressions: includeWhen,
		namespaced:             isNamespaced,
		order:                  order,
	}, nil
}

// buildDependencyGraph builds the dependency graph between the resources in the
// resource graph definition. The dependency graph is an directed acyclic graph that represents
// the relationships between the resources in the resource graph definition. The graph is used
// to determine the order in which the resources should be created in the cluster.
//
// This function returns the DAG, and a map of runtime variables per resource. later
// on we'll use this map to resolve the runtime variables.
func (b *Builder) buildDependencyGraph(
	resources map[string]*Resource,
) (
	// directed acyclic graph
	*dag.DirectedAcyclicGraph[string],
	// map of runtime variables per resource
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
		for _, resourceVariable := range resource.variables {
			for _, expression := range resourceVariable.Expressions {
				// We need to inspect the expression to understand how it relates to the
				// resources defined in the resource graph definition.
				err := validateCELExpressionContext(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to validate expression context: %w", err)
				}

				// We need to extract the dependencies from the expression.
				resourceDependencies, isStatic, err := extractDependencies(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies: %w", err)
				}

				// Static until proven dynamic.
				//
				// This reads as: If the expression is dynamic and the resource variable is
				// static, then we need to mark the resource variable as dynamic.
				if !isStatic && resourceVariable.Kind == variable.ResourceVariableKindStatic {
					resourceVariable.Kind = variable.ResourceVariableKindDynamic
				}

				resource.addDependencies(resourceDependencies...)
				resourceVariable.AddDependencies(resourceDependencies...)
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
	// CRDs, it doesn't have an OpenAPI schema. Instead, it has a schema defined
	// using the "SimpleSchema" format, a new standard we created to simplify
	// CRD declarations.

	// The instance resource is a Kubernetes resource, so it has a GroupVersionKind.
	gvk := metadata.GetResourceGraphDefinitionInstanceGVK(group, apiVersion, kind)

	// We need to unmarshal the instance schema to a map[string]interface{} to
	// make it easier to work with.
	unstructuredInstance := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgDefinition.Spec.Raw, &unstructuredInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal instance schema: %w", err)
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
	instanceCRD := crd.SynthesizeCRD(group, apiVersion, kind, *instanceSpecSchema, *instanceStatusSchema, overrideStatusFields)

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
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
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
func buildInstanceSpecSchema(rgSchema *v1alpha1.Schema) (*extv1.JSONSchemaProps, error) {
	// We need to unmarshal the instance schema to a map[string]interface{} to
	// make it easier to work with.
	instanceSpec := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgSchema.Spec.Raw, &instanceSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec schema: %w", err)
	}

	// The instance resource has a schema defined using the "SimpleSchema" format.
	instanceSchema, err := simpleschema.ToOpenAPISpec(instanceSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %v", err)
	}
	return instanceSchema, nil
}

// buildStatusSchema builds the status schema for the instance resource. The
// status schema is inferred from the CEL expressions in the status field.
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

	// different from the instance spec, the status schema is inferred from the
	// CEL expressions in the status field.
	fieldDescriptors, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	// Inspection of the CEL expressions to infer the types of the status fields.
	resourceNames := maps.Keys(resources)

	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
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
			// resources defined in the resource graph definition.
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
// of the resources defined in the resource graph definition.
func validateCELExpressionContext(env *cel.Env, expression string, resources []string) error {
	inspector := ast.NewInspectorWithEnv(env, resources, nil)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource graph definition.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return fmt.Errorf("failed to inspect expression: %w", err)
	}
	// make sure that the expression refers to the resources defined in the resource graph definition.
	for _, resource := range inspectionResult.ResourceDependencies {
		if !slices.Contains(resources, resource.ID) {
			return fmt.Errorf("expression refers to unknown resource: %s", resource.ID)
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

// validateResourceCELExpressions tries to validate the CEL expressions in the
// resources against the resources defined in the resource graph definition.
//
// In this process, we pin a resource and evaluate the CEL expressions in the
// context of emulated resources. Meaning that given 3 resources A, B, and C,
// we evalute A's CEL expressions against 2 emulated resources B and C. Then
// we evaluate B's CEL expressions against 2 emulated resources A and C, and so
// on.
func validateResourceCELExpressions(resources map[string]*Resource, instance *Resource) error {
	resourceNames := maps.Keys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "schema")
	conditionFieldNames := []string{"schema"}

	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
	if err != nil {
		return fmt.Errorf("failed to create CEL environment: %w", err)
	}
	instanceEmulatedCopy := instance.emulatedObject.DeepCopy()
	if instanceEmulatedCopy != nil && instanceEmulatedCopy.Object != nil {
		delete(instanceEmulatedCopy.Object, "apiVersion")
		delete(instanceEmulatedCopy.Object, "kind")
		delete(instanceEmulatedCopy.Object, "status")
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
				context["schema"] = &Resource{
					emulatedObject: &unstructured.Unstructured{
						Object: instanceEmulatedCopy.Object,
					},
				}

				_, err = dryRunExpression(env, expression, context)
				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", expression, err)
				}
			}

			// validate readyWhen Expressions for resource
			// Only accepting expressions accessing the status and spec for now
			// and need to evaluate to a boolean type
			//
			// TODO(michaelhtm) It shares some of the logic with the loop from above..maybe
			// we can refactor them or put it in one function.
			// I would also suggest separating the dryRuns of readyWhenExpressions
			// and the resourceExpressions.
			for _, readyWhenExpression := range resource.readyWhenExpressions {
				fieldEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{resource.id}))
				if err != nil {
					return fmt.Errorf("failed to create CEL environment: %w", err)
				}

				err = validateCELExpressionContext(fieldEnv, readyWhenExpression, []string{resource.id})
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", readyWhenExpression, err)
				}
				// create context
				// add resource fields to the context
				resourceEmulatedCopy := resource.emulatedObject.DeepCopy()
				if resourceEmulatedCopy != nil && resourceEmulatedCopy.Object != nil {
					delete(resourceEmulatedCopy.Object, "apiVersion")
					delete(resourceEmulatedCopy.Object, "kind")
				}
				context := map[string]*Resource{}
				context[resource.id] = &Resource{
					emulatedObject: resourceEmulatedCopy,
				}
				output, err := dryRunExpression(fieldEnv, readyWhenExpression, context)

				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", readyWhenExpression, err)
				}
				if !krocel.IsBoolType(output) {
					return fmt.Errorf("output of readyWhen expression %s can only be of type bool", readyWhenExpression)
				}
			}

			for _, includeWhenExpression := range resource.includeWhenExpressions {
				instanceEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
				if err != nil {
					return fmt.Errorf("failed to create CEL environment: %w", err)
				}

				err = validateCELExpressionContext(instanceEnv, includeWhenExpression, conditionFieldNames)
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", includeWhenExpression, err)
				}
				// create context
				context := map[string]*Resource{}
				// for now we will only support the instance context for condition expressions.
				// With this decision we will decide in creation time, and update time
				// If we'll be creating resources or not
				context["schema"] = &Resource{
					emulatedObject: &unstructured.Unstructured{
						Object: instanceEmulatedCopy.Object,
					},
				}

				output, err := dryRunExpression(instanceEnv, includeWhenExpression, context)
				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", includeWhenExpression, err)
				}
				if !krocel.IsBoolType(output) {
					return fmt.Errorf("output of condition expression %s can only be of type bool", includeWhenExpression)
				}
			}
		}
	}

	return nil
}
