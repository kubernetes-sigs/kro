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

package resourcegroup

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types/ref"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sSchema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"github.com/aws-controllers-k8s/symphony/api/v1alpha1"
	"github.com/aws-controllers-k8s/symphony/internal/celutil"
	"github.com/aws-controllers-k8s/symphony/internal/dag"
	"github.com/aws-controllers-k8s/symphony/internal/k8smetadata"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup/crd"
	"github.com/aws-controllers-k8s/symphony/internal/resourcegroup/schema"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/celinspector"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/emulator"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/parser"
	"github.com/aws-controllers-k8s/symphony/internal/typesystem/simpleschema"
)

func NewResourceGroupBuilder(
	clientConfig *rest.Config,
) (*GraphBuilder, error) {
	schemaResolver, dc, err := schema.NewCombinedResolver(clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema resolver: %w", err)
	}

	resourceEmulator := emulator.NewEmulator()

	rgBuilder := &GraphBuilder{
		resourceEmulator: resourceEmulator,
		schemaResolver:   schemaResolver,
		discoveryClient:  dc,
	}
	return rgBuilder, nil
}

type GraphBuilder struct {
	schemaResolver   resolver.SchemaResolver
	resourceEmulator *emulator.Emulator
	discoveryClient  *discovery.DiscoveryClient
}

func (b *GraphBuilder) NewResourceGroup(rg *v1alpha1.ResourceGroup) (*ResourceGroup, error) {
	// Before anything else, let's copy the resource group to avoid modifying the
	// original object.
	resourceGroupCR := rg.DeepCopy()

	// There are a few steps to build a resource group:
	// 1. Validate the naming convention of the resource group and its resources.
	//    Symphony leverages CEL expressions to allow users to define new types and
	//    express relationships between resources. This means that we need to ensure
	//    that the names of the resources are valid to be used in CEL expressions.
	//    for example name-something-something is not a valid name for a resource,
	//    because in CEL - is a subtraction operator.
	err := validateRGResourceNames(resourceGroupCR)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resourcegroup.spec.resources names: %w", err)
	}

	resourceNames := make([]string, 0, len(resourceGroupCR.Spec.Resources))
	for _, rgResource := range resourceGroupCR.Spec.Resources {
		resourceNames = append(resourceNames, rgResource.Name)
	}

	// Now that did a basic validation of the resource group, we can start understanding
	// the resources that are part of the resource group.

	// For each resource in the resource group, we need to:
	// 1. Check if it looks like a valid Kubernetes resource. This means that it
	//    has a group, version, and kind, and a metadata field.
	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	// 3. Extract CEL expressions from the schema.
	// 4. Build the resource object.

	namespacedResources := map[k8sSchema.GroupVersionKind]bool{}

	apiResourceList, err := b.discoveryClient.ServerPreferredNamespacedResources()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve Kubernetes namespaced resources: %w", err)
	}

	for _, resourceList := range apiResourceList {
		for _, r := range resourceList.APIResources {
			gvk := k8sSchema.FromAPIVersionAndKind(resourceList.GroupVersion, r.Kind)
			namespacedResources[gvk] = r.Namespaced
		}
	}

	// we'll also store the resources in a map for easy access later.
	resources := make(map[string]*Resource)
	for _, rgResource := range resourceGroupCR.Spec.Resources {
		// 1. We need to unmashal the resource into a map[string]interface{} to
		//    make it easier to work with.
		unstructuredResource := map[string]interface{}{}
		err := yaml.UnmarshalStrict([]byte(rgResource.Definition.Raw), &unstructuredResource)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.Name, err)
		}

		// 1. Check if it looks like a valid Kubernetes resource.
		//    TODO(a-hilaly): reason as an error? what is the reason?
		err = validateKubernetesObjectStructure(unstructuredResource)
		if err != nil {
			return nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rgResource.Name, err)
		}

		// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
		gvk, err := k8smetadata.ExtractGVKFromUnstructured(unstructuredResource)
		if err != nil {
			return nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rgResource.Name, err)
		}

		// 3. Load the OpenAPI schema for the resource.
		resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
		if err != nil {
			return nil, fmt.Errorf("failed to get schema for resource %s: %w", rgResource.Name, err)
		}

		var emulatedResource *unstructured.Unstructured
		var resourceVariables []*ResourceVariable

		// TODO(michaelhtm): CRDs are not supported for extraction currently
		// implement new logic specific to CRDs
		if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
			celExpressions, err := parser.ParseSchemalessResource(unstructuredResource)
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

			// 5. Extract CEL celExpressions from the schema.
			celExpressions, err := parser.ParseResource(unstructuredResource, resourceSchema)
			if err != nil {
				return nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.Name, err)
			}
			for _, celExpression := range celExpressions {
				resourceVariables = append(resourceVariables, &ResourceVariable{
					// Assume variables are static, we'll validate them later
					Kind:     ResourceVariableKindStatic,
					CELField: celExpression,
				})
			}
		}

		_, isNamespaced := namespacedResources[gvk]

		resources[rgResource.Name] = &Resource{
			ID:               rgResource.Name,
			GroupVersionKind: gvk,
			Schema:           resourceSchema,
			EmulatedObject:   emulatedResource,
			OriginalObject:   unstructuredResource,
			Variables:        resourceVariables,
			Namespaced:       isNamespaced,
		}
	}

	// At this stage we have a superficial understanding of the resources that are
	// part of the resource group. We have the OpenAPI schema for each resource, and
	// we have extracted the CEL expressions from the schema.
	//
	// Next, we need to understand the instance resource. The instance resource is
	// the resource users will create in their cluster, to request the creation of
	// the resources defined in the resource group.

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

	ownerReference := k8smetadata.NewResourceGroupOwnerReference(rg.Name, rg.UID)
	instance, err := b.buildInstanceResource(
		resourceGroupCR.Spec.APIVersion,
		resourceGroupCR.Spec.Kind,
		ownerReference,
		resourceGroupCR.Spec.Definition,
		resources,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build resourcegroup %v: %w", err, nil)
	}

	// Before getting into the dependency graph, we need to validate the CEL expressions
	// in the instance resource. In order to do that, we need to isolate each resource
	// and evaluate the CEL expressions in the context of the resource group. This is done
	// by dry-running the CEL expressions against the emulated resources.
	err = b.validateResourceCELExpressions(resources, instance)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resource CEL expressions: %w", err)
	}

	// Now that we have the instance resource, we can move into the next stage of
	// building the resource group. Understanding the relationships between the
	// resources in the resource group a.k.a the dependency graph.
	//
	// The dependency graph is an acyclic graph that represents the relationships
	// between the resources in the resource group. The graph is used to determine
	// the order in which the resources should be created in the cluster.
	//
	// The dependency graph is built by inspecting the CEL expressions in the
	// resources and the instance resource. The CEL expressions are used to
	// express relationships between resources, and Symphony uses this information
	// to build the dependency graph.

	dag, resourceDependencies, runtimeVariables, err := b.buildDependencyGraph(resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}

	allResources := make([]string, 0, len(resources))
	for resourceName, dependencies := range resourceDependencies {
		resources[resourceName].Dependencies = dependencies
		allResources = append(allResources, resourceName)
	}

	// merge the instance variables with the resource variables
	instanceRuntimeVariables := []*RuntimeVariable{}
	for _, variable := range instance.Variables {
		instanceRuntimeVariables = append(instanceRuntimeVariables, &RuntimeVariable{
			Expression:   variable.CELField.Expressions[0],
			Kind:         variable.Kind,
			Dependencies: allResources,
			Resolved:     false,
		})
	}
	runtimeVariables["instance"] = instanceRuntimeVariables

	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to get topological order: %w", err)
	}
	resourceGroup := &ResourceGroup{
		Dag:              dag,
		Instance:         instance,
		Resources:        resources,
		RuntimeVariables: runtimeVariables,
		TopologicalOrder: topologicalOrder,
		ResourceNames:    resourceNames,
	}
	return resourceGroup, nil
}

func getMapKeys[T comparable, K any](m map[T]K) []T {
	keys := make([]T, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func (b *GraphBuilder) validateResourceCELExpressions(resources map[string]*Resource, instance *Resource) error {
	resourceNames := getMapKeys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "spec")

	env, err := celutil.NewEnvironement(celutil.WithResourceNames(resourceNames))
	if err != nil {
		return fmt.Errorf("failed to create CEL environment: %w", err)
	}

	for _, resource := range resources {
		for _, variable := range resource.Variables {
			for _, expression := range variable.Expressions {
				err := b.validateCELExpressionContext(env, expression, resourceNames)
				if err != nil {
					return fmt.Errorf("failed to validate expression context: '%s' %w", expression, err)
				}

				// create context
				context := map[string]*Resource{}
				for resourceName, contextResource := range resources {
					// exclude the resource we are validating
					if resourceName != resource.ID {
						context[resourceName] = contextResource
					}
				}
				// add instance spec to the context
				context["spec"] = &Resource{
					ID:               "spec",
					GroupVersionKind: instance.GroupVersionKind,
					Schema:           instance.Schema,
					EmulatedObject: &unstructured.Unstructured{
						Object: instance.EmulatedObject.Object["spec"].(map[string]interface{}),
					},
				}

				_, err = b.dryRunExpression(env, expression, context)
				if err != nil {
					return fmt.Errorf("failed to dry-run expression %s: %w", expression, err)
				}
			}
		}
	}

	return nil
}

func (b *GraphBuilder) buildDependencyGraph(
	resources map[string]*Resource,
) (
	*dag.DirectedAcyclicGraph,
	map[string][]string,
	map[string][]*RuntimeVariable,
	error,
) {
	dependencyMap := make(map[string][]string)

	resourceNames := getMapKeys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "spec")

	env, err := celutil.NewEnvironement(celutil.WithResourceNames(resourceNames))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	directedAcyclicGraph := dag.NewDirectedAcyclicGraph()
	// Set the vertices of the graph to be the resources defined in the resource group.
	for resourceName := range resources {
		if err := directedAcyclicGraph.AddVertex(resourceName); err != nil {
			return nil, nil, nil, fmt.Errorf("failed to add vertex to graph: %w", err)
		}
	}

	runtimeVariablesPerResource := make(map[string][]*RuntimeVariable)

	for resourceName, resource := range resources {
		dependencyMap[resourceName] = make([]string, 0)

		for _, variable := range resource.Variables {
			for _, expression := range variable.Expressions {
				// We need to inspect the expression to understand how it relates to the
				// resources defined in the resource group.
				err := b.validateCELExpressionContext(env, expression, resourceNames)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("failed to validate expression context: %w", err)
				}

				// We need to extract the dependencies from the expression.
				resourceDependencies, isStatic, err := b.extractDependencies(env, expression, resources)
				if err != nil {
					return nil, nil, nil, fmt.Errorf("failed to extract dependencies: %w", err)
				}
				if !isStatic {
					// If we have seen a dynamic dependency, we need to mark the variable as dynamic.
					variable.Kind = ResourceVariableKindDynamic
				}

				// We need to add the dependencies to the graph.
				for _, dependency := range resourceDependencies {
					if err := directedAcyclicGraph.AddEdge(resourceName, dependency); err != nil {
						return nil, nil, nil, err
					}
					if !inSlice(dependency, dependencyMap[resourceName]) {
						dependencyMap[resourceName] = append(dependencyMap[resourceName], dependency)
					}
				}

				runtimeVariablesPerResource[resourceName] = append(runtimeVariablesPerResource[resourceName], &RuntimeVariable{
					Expression:   expression,
					Kind:         variable.Kind,
					Dependencies: resourceDependencies,
					Resolved:     false,
				})
			}
		}
	}

	return directedAcyclicGraph, dependencyMap, runtimeVariablesPerResource, nil
}

func (b *GraphBuilder) extractDependencies(env *cel.Env, expression string, resources map[string]*Resource) ([]string, bool, error) {
	resourceNames := getMapKeys(resources)
	// We also want to allow users to refer to the instance spec in their expressions.
	resourceNames = append(resourceNames, "spec")

	inspector := celinspector.NewInspectorWithEnv(env, resourceNames, nil)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource group.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect expression: %w", err)
	}

	isStatic := true
	dependencies := make([]string, 0)
	for _, resource := range inspectionResult.ResourceDependencies {
		if resource.Name != "spec" && !inSlice(resource.Name, dependencies) {
			isStatic = false
			dependencies = append(dependencies, resource.Name)
		}
	}
	if len(inspectionResult.UnknownResources) > 0 {
		return nil, false, fmt.Errorf("found unknown resources in CEL expression: [%v]", inspectionResult.UnknownResources)
	}

	return dependencies, isStatic, nil
}

func (b *GraphBuilder) buildInstanceResource(
	apiVersion, kind string,
	ownerReference metav1.OwnerReference,
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
	gvk := k8smetadata.GetResourceGroupInstanceGVK(apiVersion, kind)

	// We need to unmarshal the instance definition to a map[string]interface{} to
	// make it easier to work with.
	unstructuredInstance := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgDefinition.Spec.Raw, &unstructuredInstance)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal instance definition: %w", err)
	}

	// The instance resource has a schema defined using the "SimpleSchema" format.
	instanceSpecSchema, err := b.buildInstanceSpecSchema(rgDefinition)
	if err != nil {
		return nil, fmt.Errorf("failed to build OpenAPI schema for instance: %w", err)
	}

	instanceStatusSchema, statusVariables, err := b.buildStatusSchema(rgDefinition, resources)
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

	// The instance resource has a set of variables that need to be resolved.
	// instance.Variables = b.parser.ExtractCELExpressions(rg.Spec.InstanceSchema)

	instanceVariables := []*ResourceVariable{}
	for _, statusVariable := range statusVariables {
		// These variables needs to be injected into the status field of the instance.
		path := ".status" + statusVariable.Path
		statusVariable.Path = path
		instanceVariables = append(instanceVariables, &ResourceVariable{
			Kind:     ResourceVariableKindDynamic,
			CELField: statusVariable,
		})
	}

	instance := &Resource{
		ID:               "instance",
		GroupVersionKind: gvk,
		Schema:           instanceSchema,
		SchemaExt:        instanceSchemaExt,
		CRD:              instanceCRD,
		EmulatedObject:   emulatedInstance,
		Variables:        instanceVariables,
	}
	return instance, nil
}

func (b *GraphBuilder) buildInstanceSpecSchema(definition *v1alpha1.Definition) (*extv1.JSONSchemaProps, error) {
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

func (b *GraphBuilder) buildStatusSchema(definition *v1alpha1.Definition, resources map[string]*Resource) (*extv1.JSONSchemaProps, []parser.CELField, error) {
	// The instance resource has a schema defined using the "SimpleSchema" format.
	unstructuredStatus := map[string]interface{}{}
	err := yaml.UnmarshalStrict(definition.Status.Raw, &unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal status definition: %w", err)
	}

	// different from the instance spec, the status schema is inferred from the
	// CEL expressions in the status field.
	extracted, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	// Inspection of the CEL expressions to infer the types of the status fields.
	resourceNames := getMapKeys(resources)

	env, err := celutil.NewEnvironement(celutil.WithResourceNames(resourceNames))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	// statusStructureParts := make([]schema.FieldDescriptor, 0, len(extracted))
	statusDryRunResults := make(map[string][]ref.Val, len(extracted))
	for _, found := range extracted {
		// For each expression in the extracted ExpressionField we need to dry-run
		// the expression to infer the type of the status field.
		evals := []ref.Val{}
		for _, expr := range found.Expressions {
			// we need to inspect the expression to understand how it relates to the
			// resources defined in the resource group.
			err := b.validateCELExpressionContext(env, expr, resourceNames)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to validate expression context: %w", err)
			}

			value, err := b.dryRunExpression(env, expr, resources)
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
	return statusSchema, extracted, nil
}

func (b *GraphBuilder) validateCELExpressionContext(env *cel.Env, expression string, resources []string) error {
	inspector := celinspector.NewInspectorWithEnv(env, resources, nil)

	// The CEL expression is valid if it refers to the resources defined in the
	// resource group.
	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return fmt.Errorf("failed to inspect expression: %w", err)
	}
	// make sure that the expression refers to the resources defined in the resource group.
	for _, resource := range inspectionResult.ResourceDependencies {
		if !inSlice(resource.Name, resources) {
			return fmt.Errorf("expression refers to unknown resource: %s", resource.Name)
		}
	}
	return nil
}

func (b *GraphBuilder) dryRunExpression(env *cel.Env, expression string, resources map[string]*Resource) (ref.Val, error) {
	// The status schema is inferred from the CEL expressions in the status field.
	ast, issues := env.Compile(expression)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to compile expression: %w", issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create program: %w", err)
	}

	context := map[string]interface{}{}
	for resourceName, resource := range resources {
		context[resourceName] = resource.EmulatedObject.Object
	}

	output, _, err := program.Eval(context)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate expression: %w", err)
	}
	return output, nil
}
