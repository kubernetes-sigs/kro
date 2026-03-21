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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	celcache "github.com/kubernetes-sigs/kro/pkg/cel/cache"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/features"
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

	schemaCache := schema.NewCache()
	rgBuilder := &Builder{
		schemaResolver: schemaResolver,
		restMapper:     rm,
		celCache:       celcache.NewBuilderCache(),
		schemaCache:    schemaCache,
		parser:         parser.New(schemaCache),
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
	// celCache holds cached CEL compilation artifacts (DeclTypes, typed
	// environments, field type maps) scoped to this Builder instance.
	// Long-lived across reconciles for cross-RGD cache hits.
	celCache    *celcache.BuilderCache
	schemaCache *schema.Cache
	parser      *parser.Parser
}

// RGDConfig holds RGD runtime configuration parameters.
type RGDConfig struct {
	MaxCollectionSize          int
	MaxCollectionDimensionSize int
}

// NewResourceGraphDefinition creates a new ResourceGraphDefinition object from the given ResourceGraphDefinition
// CRD. The ResourceGraphDefinition object is a fully processed and validated representation
// of the resource graph definition CRD, it's underlying resources, and the relationships between
// the resources.
func (b *Builder) NewResourceGraphDefinition(originalCR *v1alpha1.ResourceGraphDefinition, rgdConfig RGDConfig) (*Graph, error) {
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
	err := validateResourceGraphDefinition(rgd, rgdConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resourcegraphdefinition: %w", err)
	}

	// Determine CRD scope from the schema definition. Defaults to NamespaceScoped
	// to preserve backward compatibility.
	crdScope := extv1.NamespaceScoped
	if rgd.Spec.Schema.Scope == v1alpha1.ResourceScopeCluster {
		crdScope = extv1.ClusterScoped
	}
	instanceNamespaced := crdScope == extv1.NamespaceScoped

	// Now that we did a basic validation of the resource graph definition, we can start understanding
	// the resources that are part of the resource graph definition.

	// For each resource in the resource graph definition, we need to:
	// 1. Check if it looks like a valid Kubernetes resource. This means that it
	//    has a group, version, and kind, and a metadata field.
	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	// 3. Emulate the resource, this is later used to verify the validity of the
	//    CEL expressions.
	// 4. Extract the CEL expressions from the resource + validate them.

	// we'll also store the nodes and schemas in maps for easy access later.
	// Schemas are only needed during build for CEL validation.
	nodes := make(map[string]*Node)
	schemas := make(map[string]*spec.Schema)
	for i, rgResource := range rgd.Spec.Resources {
		id := rgResource.ID
		node, nodeSchema, err := b.buildRGResource(rgResource, i, instanceNamespaced)
		if err != nil {
			return nil, fmt.Errorf("failed to build resource %q: %w", id, err)
		}
		if nodes[id] != nil {
			return nil, fmt.Errorf("found resources with duplicate id %q", id)
		}
		nodes[id] = node
		schemas[id] = nodeSchema
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

	// Build instance spec schema from SimpleSchema.
	// This is independent of resources - just YAML parsing.
	instanceSpecSchema, err := buildInstanceSpecSchema(rgd.Spec.Schema)
	if err != nil {
		return nil, fmt.Errorf("failed to build resourcegraphdefinition %q: %w", rgd.Name, err)
	}

	// Synthesize CRD early with empty status.
	// We'll update the status later after inferring it from CEL expressions.
	instanceCRD := crd.SynthesizeCRD(
		rgd.Spec.Schema.Group,
		rgd.Spec.Schema.APIVersion,
		rgd.Spec.Schema.Kind,
		*instanceSpecSchema,
		extv1.JSONSchemaProps{}, // empty status placeholder
		false,                   // don't add default fields yet
		crdScope,
		rgd.Spec.Schema,
	)

	// Create a single expression inspector for all AST inspection operations.
	// This uses a lightweight env that only declares identifier names (no full schemas) -
	// sufficient for parsing and finding references, but NOT for type-checking or compilation.
	nodeNames := maps.Keys(nodes)
	allIdentifiers := append(nodeNames, SchemaVarName, EachVarName)
	inspectorEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(allIdentifiers))
	if err != nil {
		return nil, fmt.Errorf("failed to create inspector environment: %w", err)
	}
	inspector := ast.NewInspectorWithEnv(inspectorEnv, allIdentifiers)

	// Build the dependency graph by inspecting CEL expressions.
	// This extracts all resource dependencies and validates that:
	// 1. All referenced resources are defined in the RGD
	// 2. There are no unknown functions
	// 3. The dependency graph is acyclic
	//
	// We do this BEFORE type checking so that undeclared resource errors
	// are caught here with clear messages, rather than as CEL type errors.
	dag, err := b.buildDependencyGraph(nodes, inspector)
	if err != nil {
		return nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}
	// Ensure the graph is acyclic and get the topological order of resources.
	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to get topological order: %w", err)
	}

	// Validate that omit() is not used on resource identity fields.
	// Only needed when omit() is enabled — if the gate is off, the builder
	// already rejects any omit() usage in extractDependencies.
	if features.FeatureGate.Enabled(features.CELOmitFunction) {
		if err := validateIdentityFields(nodes, inspector, instanceNamespaced); err != nil {
			return nil, err
		}
	}

	// Collect all schemas for CEL validation:
	// - Resource schemas (wrapped as lists for collections)
	// - Instance spec schema as "schema" variable (extracted from CRD, without status)
	//
	// This allows expressions like ${schema.spec.replicas} and ${deployment.status.replicas}.
	// Note: only spec and metadata are included - status references are not allowed in RGDs.
	celSchemas := collectNodeSchemas(b.schemaCache, nodes, schemas)
	schemaWithoutStatus, err := getSchemaWithoutStatus(instanceCRD)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema without status: %w", err)
	}
	celSchemas[SchemaVarName] = schemaWithoutStatus

	// Create a single typed CEL environment with all schemas for compilation.
	// TypedEnvironmentWithProvider returns both the env and the DeclTypeProvider it
	// creates internally, avoiding duplicate schema-to-DeclType conversions.
	typedEnv, typeProvider, err := krocel.TypedEnvironmentWithProvider(b.celCache, celSchemas)
	if err != nil {
		return nil, fmt.Errorf("failed to create typed CEL environment: %w", err)
	}

	// Session cache for per-build CEL artifacts (programs, ASTs, extended envs).
	// Created fresh per build so RGD-specific artifacts don't accumulate on
	// the long-lived Builder.
	sessionCache := celcache.NewSessionCache()

	// Validate and compile all resource CEL expressions.
	for id, node := range nodes {
		if err := validateAndCompileNode(b.celCache, b.schemaCache, sessionCache, node, inspector, typedEnv, schemas[id], typeProvider); err != nil {
			return nil, fmt.Errorf("failed to validate resource %q: %w", id, err)
		}
	}

	// Build instance status schema.
	// Status expressions reference resources (validated to not reference schema).
	// We infer the status field types from the CEL expression output types.
	statusSchema, statusVariables, statusTemplate, err := buildStatusSchema(
		sessionCache,
		rgd.Spec.Schema,
		nodeNames,
		inspector,
		typedEnv,
		typeProvider,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build instance status schema: %w", err)
	}

	// Compile programs for status expressions in a separate pass.
	// buildStatusSchema only parsed and type-checked (cheap); now we compile
	// programs (expensive) using the cached ASTs.
	for _, fd := range statusVariables {
		if _, err := parseCheckAndCompile(sessionCache, typedEnv, fd.Expression); err != nil {
			return nil, fmt.Errorf("failed to compile status expression %q at path %q: %w", fd.Expression.UserExpression(), fd.Path, err)
		}
	}

	// Update the CRD with the inferred status schema.
	crd.SetCRDStatus(instanceCRD, *statusSchema, true)

	// Create the instance node with status variables for runtime patching.
	instance, err := buildInstanceNode(
		rgd.Spec.Schema.Group,
		rgd.Spec.Schema.APIVersion,
		rgd.Spec.Schema.Kind,
		instanceNamespaced,
		statusVariables,
		statusTemplate,
		inspector,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create instance node: %w", err)
	}

	// Build resource schemas map for runtime CEL value conversion.
	// Include both resource schemas and the instance schema (without status).
	resourceSchemas := make(map[string]*spec.Schema, len(schemas)+1)
	for id, sch := range schemas {
		resourceSchemas[id] = sch
	}
	resourceSchemas[InstanceNodeID] = schemaWithoutStatus

	resourceGraphDefinition := &Graph{
		DAG:              dag,
		Instance:         instance,
		Nodes:            nodes,
		Resources:        nodes,
		TopologicalOrder: topologicalOrder,
		CRD:              instanceCRD,
		ResourceSchemas:  resourceSchemas,
	}
	return resourceGraphDefinition, nil
}

// buildExternalRefResource builds an empty resource with metadata from the given externalRef definition.
// The selector (if any) is embedded directly in the template so that ParseSchemalessResource
// can extract CEL expressions from the entire resource in a single pass.
func (b *Builder) buildExternalRefResource(
	externalRef *v1alpha1.ExternalRef) (map[string]interface{}, error) {
	result, err := runtime.DefaultUnstructuredConverter.ToUnstructured(externalRef)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ExternalRef to unstructured: %w", err)
	}
	return result, nil
}

// buildRGResource builds a node from the given resource definition.
// It provides a high-level understanding of the resource, by extracting the
// OpenAPI schema, emulating the resource and extracting the cel expressions
// from the schema.
// Returns the Node and the OpenAPI schema (schema is only needed during build for CEL validation).
func (b *Builder) buildRGResource(
	rgResource *v1alpha1.Resource,
	order int,
	instanceNamespaced bool,
) (*Node, *spec.Schema, error) {
	// 1. Validate resource field combinations.
	if err := validateCombinableResourceFields(rgResource); err != nil {
		return nil, nil, fmt.Errorf("invalid combination of resource fields: %w", err)
	}

	// 2. Unmarshal the resource into a map[string]interface{}.
	resourceObject := map[string]interface{}{}
	if len(rgResource.Template.Raw) > 0 {
		err := yaml.UnmarshalStrict(rgResource.Template.Raw, &resourceObject)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.ID, err)
		}
	} else {
		var err error
		if resourceObject, err = b.buildExternalRefResource(rgResource.ExternalRef); err != nil {
			return nil, nil, fmt.Errorf("failed to build external ref resource %s: %w", rgResource.ID, err)
		}
	}

	// 3. Check if it looks like a valid Kubernetes resource.
	err := validateKubernetesObjectStructure(resourceObject)
	if err != nil {
		return nil, nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rgResource.ID, err)
	}

	// 4. Extract the GVK from the resource.
	gvk, err := metadata.ExtractGVKFromUnstructured(resourceObject)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rgResource.ID, err)
	}

	// 5. Load the OpenAPI schema for the resource.
	resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get schema for resource %s: %w", rgResource.ID, err)
	}

	mapping, err := b.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get REST mapping for resource %s: %w", rgResource.ID, err)
	}
	if err := validateTemplateConstraints(
		rgResource,
		resourceObject,
		mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		instanceNamespaced,
	); err != nil {
		return nil, nil, err
	}

	// 6. Extract CEL fieldDescriptors from the resource.
	var fieldDescriptors []variable.FieldDescriptor
	if rgResource.ExternalRef != nil {
		// External ref templates are synthetic (not user YAML), so use the
		// schemaless parser for the entire resource uniformly.
		fieldDescriptors, _, err = parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse external ref resource %s: %w", rgResource.ID, err)
		}
	} else if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		fieldDescriptors, _, err = parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rgResource.ID, err)
		}

		for _, expr := range fieldDescriptors {
			if !strings.HasPrefix(expr.Path, "metadata.") {
				return nil, nil, fmt.Errorf("CEL expressions in CRDs are only supported for metadata fields, found in path %q, resource %s", expr.Path, rgResource.ID)
			}
		}
	} else {
		fieldDescriptors, err = b.parser.ParseResource(resourceObject, resourceSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.ID, err)
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

	// 7. Parse ReadyWhen expressions
	readyWhen, err := parser.ParseConditionExpressions(rgResource.ReadyWhen)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse readyWhen expressions: %v", err)
	}

	// 8. Parse condition expressions
	includeWhen, err := parser.ParseConditionExpressions(rgResource.IncludeWhen)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse includeWhen expressions: %v", err)
	}

	// 9. Parse forEach dimensions
	forEachDimensions, err := parseForEachDimensions(rgResource.ForEach)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse forEach dimensions: %v", err)
	}

	// Determine node type.
	nodeType := NodeTypeResource
	if rgResource.ExternalRef != nil {
		if rgResource.ExternalRef.Metadata.Selector != nil {
			nodeType = NodeTypeExternalCollection
		} else {
			nodeType = NodeTypeExternal
		}
	} else if len(forEachDimensions) > 0 {
		nodeType = NodeTypeCollection
	}

	// Note that dependencies are not set here - they're extracted later in buildDependencyGraph.
	node := &Node{
		Meta: NodeMeta{
			ID:         rgResource.ID,
			Index:      order,
			Type:       nodeType,
			GVR:        mapping.Resource,
			Namespaced: mapping.Scope.Name() == meta.RESTScopeNameNamespace,
			// Dependencies will be set by buildDependencyGraph
		},
		Template:    &unstructured.Unstructured{Object: resourceObject},
		Variables:   templateVariables,
		IncludeWhen: includeWhen,
		ReadyWhen:   readyWhen,
		ForEach:     forEachDimensions,
	}
	return node, resourceSchema, nil
}

// buildDependencyGraph builds the dependency graph between the nodes in the
// resource graph definition. The dependency graph is a directed acyclic graph
// that represents the relationships between the nodes. The graph is used
// to determine the order in which the resources should be created in the cluster.
func (b *Builder) buildDependencyGraph(
	nodes map[string]*Node,
	inspector *ast.Inspector,
) (
	*dag.DirectedAcyclicGraph[string], // directed acyclic graph
	error,
) {
	directedAcyclicGraph := dag.NewDirectedAcyclicGraph[string]()
	for _, node := range nodes {
		if err := directedAcyclicGraph.AddVertex(node.Meta.ID, node.Meta.Index); err != nil {
			return nil, fmt.Errorf("failed to add vertex to graph: %w", err)
		}
	}

	for _, node := range nodes {
		iteratorNames := collectIteratorNames(node)

		// Phase 1: Extract dependencies and classify variables
		templateDeps, usedIterators, err := extractTemplateDependencies(inspector, node, iteratorNames)
		if err != nil {
			return nil, err
		}

		// Validate that all forEach dimensions are used in resource identity fields.
		if len(iteratorNames) > 0 {
			var missing []string
			for _, iterName := range iteratorNames {
				if !slices.Contains(usedIterators, iterName) {
					missing = append(missing, iterName)
				}
			}
			if len(missing) > 0 {
				return nil, fmt.Errorf(
					"node %q: all forEach dimensions must be used to produce a unique resource identity, missing: %v",
					node.Meta.ID, missing,
				)
			}
		}

		forEachDeps, err := extractForEachDependencies(inspector, node, iteratorNames)
		if err != nil {
			return nil, err
		}

		includeWhenDeps, err := extractConditionDependencies(inspector, node.IncludeWhen)
		if err != nil {
			return nil, fmt.Errorf("failed to extract dependencies from includeWhen: %w", err)
		}

		// Add all dependencies to node and DAG
		allDeps := make([]string, 0, len(templateDeps)+len(forEachDeps)+len(includeWhenDeps))
		allDeps = append(allDeps, templateDeps...)
		allDeps = append(allDeps, forEachDeps...)
		allDeps = append(allDeps, includeWhenDeps...)
		node.Meta.Dependencies = append(node.Meta.Dependencies, allDeps...)
		if err := directedAcyclicGraph.AddDependencies(node.Meta.ID, allDeps); err != nil {
			return nil, err
		}
	}

	return directedAcyclicGraph, nil
}

// collectIteratorNames returns the iterator variable names for a node's forEach.
func collectIteratorNames(node *Node) []string {
	names := make([]string, 0, len(node.ForEach))
	for _, iter := range node.ForEach {
		names = append(names, iter.Name)
	}
	return names
}

// extractTemplateDependencies extracts dependencies from template variable expressions.
// It also classifies each variable's Kind (Static -> Dynamic -> Iteration) and adds
// dependencies to each variable.
// Returns: (resourceDeps, iteratorsInIdentity, error)
// iteratorsInIdentity contains iterators used in identity fields:
//   - For namespaced resources: metadata.name or metadata.namespace
//   - For cluster-scoped resources: metadata.name only
func extractTemplateDependencies(
	inspector *ast.Inspector,
	node *Node,
	iteratorNames []string,
) ([]string, []string, error) {
	var allDeps []string
	var iteratorsInIdentity []string

	for _, templateVariable := range node.Variables {
		expression := templateVariable.Expression
		nodeDeps, iteratorRefs, err := extractDependencies(inspector, expression, iteratorNames)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to extract dependencies: %w", err)
		}

		// Promote variable Kind based on expression references.
		// Variables start as Static and get promoted: Static -> Dynamic -> Iteration.
		// The Kind == Static check prevents downgrading if a previous expression
		// already promoted it to a higher kind.
		if len(iteratorRefs) > 0 {
			templateVariable.Kind = variable.ResourceVariableKindIteration
		} else if len(nodeDeps) > 0 && templateVariable.Kind == variable.ResourceVariableKindStatic {
			templateVariable.Kind = variable.ResourceVariableKindDynamic
		}

		// Dependencies are tracked in Expression.References
		allDeps = append(allDeps, nodeDeps...)

		// Track iterators used in identity fields (name/namespace).
		switch templateVariable.Path {
		case MetadataNamePath:
			for _, iter := range iteratorRefs {
				if !slices.Contains(iteratorsInIdentity, iter) {
					iteratorsInIdentity = append(iteratorsInIdentity, iter)
				}
			}
		case MetadataNamespacePath:
			if node.Meta.Namespaced {
				for _, iter := range iteratorRefs {
					if !slices.Contains(iteratorsInIdentity, iter) {
						iteratorsInIdentity = append(iteratorsInIdentity, iter)
					}
				}
			}
		}
	}

	return allDeps, iteratorsInIdentity, nil
}

// extractForEachDependencies extracts dependencies from forEach expressions.
// If a forEach expression references another node (e.g ${config.data.items}
// or ${otherCollection}), that node becomes a DAG dependency.
// Iterator variables used in templates (e.g ${item}) are NOT DAG dependencies -
// they're local bindings resolved during ExpandCollection.
func extractForEachDependencies(
	inspector *ast.Inspector,
	node *Node,
	iteratorNames []string,
) ([]string, error) {
	var allDeps []string

	for _, iter := range node.ForEach {
		// Only pass iteratorNames - we want to detect iterator cross-references.
		// schema references in forEach are valid (e.g schema.spec.regions).
		nodeDeps, iteratorRefs, err := extractDependencies(inspector, iter.Expression, iteratorNames)
		if err != nil {
			return nil, fmt.Errorf("failed to extract dependencies from forEach iterator %q: %w", iter.Name, err)
		}

		// forEach iterators cannot reference other iterators (they're independent for cartesian product)
		if len(iteratorRefs) > 0 {
			return nil, fmt.Errorf("node %q: forEach iterator %q cannot reference other iterators %v - forEach iterators are independent (cartesian product)",
				node.Meta.ID, iter.Name, iteratorRefs)
		}

		allDeps = append(allDeps, nodeDeps...)
	}

	return allDeps, nil
}

// buildInstanceNode creates the instance node from pre-computed status components.
// This is called after spec schema, status schema, and CRD have been built separately.
// Uses the shared inspectorEnv for AST inspection.
func buildInstanceNode(
	group, apiVersion, kind string,
	namespaced bool,
	statusVariables []variable.FieldDescriptor,
	statusTemplate map[string]interface{},
	inspector *ast.Inspector,
) (*Node, error) {
	gvr := metadata.GetResourceGraphDefinitionInstanceGVR(group, apiVersion, kind)

	// Collect dependencies for instance status fields
	var instanceDeps []string
	instanceStatusVariables := []*variable.ResourceField{}
	for _, statusVariable := range statusVariables {
		// These variables need to be injected into the status field of the instance.
		path := "status." + statusVariable.Path
		statusVariable.Path = path

		// Extract dependencies from the expression
		deps, _, err := extractDependencies(inspector, statusVariable.Expression, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to extract dependencies from expression %q: %w", statusVariable.Expression, err)
		}
		if len(deps) == 0 {
			return nil, fmt.Errorf("instance status field must refer to a resource: %s", statusVariable.Path)
		}
		instanceDeps = append(instanceDeps, deps...)

		instanceStatusVariables = append(instanceStatusVariables, &variable.ResourceField{
			FieldDescriptor: statusVariable,
			Kind:            variable.ResourceVariableKindDynamic,
		})
	}

	// Create the instance node.
	// Instance doesn't have IncludeWhen, ReadyWhen, or ForEach.
	instance := &Node{
		Meta: NodeMeta{
			ID:           InstanceNodeID,
			Type:         NodeTypeInstance,
			GVR:          gvr,
			Namespaced:   namespaced,
			Dependencies: instanceDeps,
		},
		Template: &unstructured.Unstructured{
			Object: map[string]interface{}{
				"status": statusTemplate,
			},
		},
		Variables: instanceStatusVariables,
	}

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
// using CEL type checking. Uses the shared inspectorEnv for validation and typed env for compilation.
// Returns: (schema, fieldDescriptors, statusTemplate, error)
func buildStatusSchema(
	sessionCache *celcache.SessionCache,
	rgSchema *v1alpha1.Schema,
	nodeNames []string,
	inspector *ast.Inspector,
	env *cel.Env,
	typeProvider *krocel.DeclTypeProvider,
) (
	*extv1.JSONSchemaProps,
	[]variable.FieldDescriptor,
	map[string]interface{},
	error,
) {
	// The instance resource has a schema defined using the "SimpleSchema" format.
	unstructuredStatus := map[string]interface{}{}
	err := yaml.UnmarshalStrict(rgSchema.Status.Raw, &unstructuredStatus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to unmarshal status schema: %w", err)
	}

	// Extract CEL expressions from the status field.
	fieldDescriptors, noExpressionFields, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	if len(noExpressionFields) > 0 {
		return nil, nil, nil, fmt.Errorf("status fields without expressions are not supported: %v", noExpressionFields)
	}

	// Instance status expressions can ONLY reference resources, not schema.
	// At runtime, status is populated after resources are created.

	// Verify status expressions don't reference schema and populate References
	for _, fieldDescriptor := range fieldDescriptors {
		expression := fieldDescriptor.Expression
		result, err := inspectExpressionRestricted(inspector, expression.Original, nodeNames)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("status field %q expression %q: %w", fieldDescriptor.Path, expression.UserExpression(), err)
		}
		// Populate expression.References for restricted environment compilation
		for _, dep := range result.ResourceDependencies {
			if !slices.Contains(expression.References, dep.ID) {
				expression.References = append(expression.References, dep.ID)
			}
		}
	}

	// Infer types for each status field expression using CEL type checking.
	// Only parse and check here (no program compilation) — programs are compiled
	// in a separate pass after buildStatusSchema returns. This separates cheap
	// schema inference from expensive program compilation.
	statusTypeMap := make(map[string]*cel.Type)
	for _, fieldDescriptor := range fieldDescriptors {
		expression := fieldDescriptor.Expression

		checkedAST, err := parseAndCheck(sessionCache, env, expression)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to type-check status expression %q at path %q: %w", expression.UserExpression(), fieldDescriptor.Path, err)
		}

		statusTypeMap[fieldDescriptor.Path] = checkedAST.OutputType()
	}

	// convert the CEL types to OpenAPI schema - best effort.
	statusSchema, err := schema.GenerateSchemaFromCELTypes(statusTypeMap, typeProvider)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to generate status schema from CEL types: %w", err)
	}

	return statusSchema, fieldDescriptors, unstructuredStatus, nil
}

// inspectExpressionRestricted uses the shared inspector to parse an expression,
// then validates that only the allowed identifiers are referenced.
// This is used for restricted contexts like includeWhen (only schema) or readyWhen (only self).
func inspectExpressionRestricted(inspector *ast.Inspector, expr string, allowedIdentifiers []string) (ast.ExpressionInspection, error) {
	result, err := inspector.Inspect(expr)
	if err != nil {
		return ast.ExpressionInspection{}, err
	}

	// Check that only allowed identifiers are referenced
	for _, dep := range result.ResourceDependencies {
		if !slices.Contains(allowedIdentifiers, dep.ID) {
			return ast.ExpressionInspection{}, fmt.Errorf("references unknown identifiers: [%s]", dep.ID)
		}
	}

	// Unknown resources are truly unknown (not in the shared inspector's known set)
	if len(result.UnknownResources) > 0 {
		var names []string
		for _, r := range result.UnknownResources {
			names = append(names, r.ID)
		}
		return ast.ExpressionInspection{}, fmt.Errorf("references unknown identifiers: %v", names)
	}
	if len(result.UnknownFunctions) > 0 {
		return ast.ExpressionInspection{}, fmt.Errorf("uses unknown functions: %v", result.UnknownFunctions)
	}
	// omit() is registered globally but is only valid in resource template
	// expressions. Reject it in restricted contexts (includeWhen, readyWhen,
	// instance status).
	if result.UsesOmit() {
		return ast.ExpressionInspection{}, fmt.Errorf("omit() can only be used in resource template expressions")
	}
	return result, nil
}

// extractDependencies extracts the dependencies from the given CEL expression.
// It returns two slices:
//   - resourceDeps: actual resource dependencies (other resources in the RGD)
//   - iteratorRefs: references to iterator variables (from forEach dimensions)
//
// Iterator variables are recognized and returned in iteratorRefs for validation.
// Also populates expr.References with all referenced identifiers.
func extractDependencies(inspector *ast.Inspector, expr *krocel.Expression, iteratorVars []string) (
	resourceDeps []string,
	iteratorRefs []string,
	err error,
) {
	inspectionResult, err := inspector.Inspect(expr.Original)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to inspect expression: %w", err)
	}

	if !features.FeatureGate.Enabled(features.CELOmitFunction) && inspectionResult.UsesOmit() {
		return nil, nil, fmt.Errorf("omit() requires the CELOmitFunction feature gate to be enabled")
	}

	// Populate expression references
	for _, dep := range inspectionResult.ResourceDependencies {
		if !slices.Contains(expr.References, dep.ID) {
			expr.References = append(expr.References, dep.ID)
		}
	}

	for _, resource := range inspectionResult.ResourceDependencies {
		// SchemaVarName is the instance spec, not a resource dependency
		if resource.ID == SchemaVarName {
			continue
		}
		// Everything else is a resource dependency
		if !slices.Contains(resourceDeps, resource.ID) {
			resourceDeps = append(resourceDeps, resource.ID)
		}
	}

	// Handle unknown resources - they might be iterator variables
	for _, unknown := range inspectionResult.UnknownResources {
		if slices.Contains(iteratorVars, unknown.ID) {
			// It's an iterator variable - track it separately
			if !slices.Contains(iteratorRefs, unknown.ID) {
				iteratorRefs = append(iteratorRefs, unknown.ID)
			}
			// Also add to references
			if !slices.Contains(expr.References, unknown.ID) {
				expr.References = append(expr.References, unknown.ID)
			}
		} else {
			// Truly unknown resource
			return nil, nil, fmt.Errorf("references unknown identifiers: [%s]", unknown.ID)
		}
	}

	if len(inspectionResult.UnknownFunctions) > 0 {
		return nil, nil, fmt.Errorf("uses unknown functions: %v", inspectionResult.UnknownFunctions)
	}
	return resourceDeps, iteratorRefs, nil
}

// extractConditionDependencies extracts resource dependencies from condition
// expressions such as includeWhen. It also populates expr.References for later
// validation.
func extractConditionDependencies(
	inspector *ast.Inspector,
	expressions []*krocel.Expression,
) ([]string, error) {
	var allDeps []string

	for _, expression := range expressions {
		nodeDeps, _, err := extractDependencies(inspector, expression, nil)
		if err != nil {
			return nil, err
		}

		for _, dep := range nodeDeps {
			if !slices.Contains(allDeps, dep) {
				allDeps = append(allDeps, dep)
			}
		}
	}

	return allDeps, nil
}

// validateConditionReferences verifies that a set of already-inspected
// condition expressions only reference the allowed identifiers.
func validateConditionReferences(expressions []*krocel.Expression, allowedIdentifiers []string) error {
	for _, expression := range expressions {
		for _, ref := range expression.References {
			if !slices.Contains(allowedIdentifiers, ref) {
				return fmt.Errorf("references unknown identifiers: [%s]", ref)
			}
		}
	}
	return nil
}

// parseForEachDimensions converts API forEach dimensions (map[string]string) to
// ForEachDimension structs. Each API dimension is a single-entry map where
// the key is the variable name and the value is the CEL expression.
func parseForEachDimensions(apiDimensions []v1alpha1.ForEachDimension) ([]ForEachDimension, error) {
	if len(apiDimensions) == 0 {
		return nil, nil
	}

	result := make([]ForEachDimension, 0, len(apiDimensions))
	for _, dimensionMap := range apiDimensions {
		// Each dimension is a map with exactly one entry
		for name, expression := range dimensionMap {
			// Parse the expression to extract the raw CEL (strip ${...} wrapper if present)
			parsedExprs, err := parser.ParseConditionExpressions([]string{expression})
			if err != nil {
				return nil, fmt.Errorf("invalid forEach expression for dimension %q: %w", name, err)
			}
			if len(parsedExprs) != 1 {
				return nil, fmt.Errorf("forEach dimension %q must have exactly one expression", name)
			}

			result = append(result, ForEachDimension{
				Name:       name,
				Expression: parsedExprs[0],
			})
		}
	}
	return result, nil
}

// resolveSchemaAndTypeName walks through path segments and returns the schema
// at that location along with a fully-qualified CEL type name.
//
// For each segment:
//   - Named segments: append to type name, look up in schema properties
//   - Index segments: dereference array to element schema, append ".@idx" to type name
func resolveSchemaAndTypeName(c *schema.Cache, segments []fieldpath.Segment, rootSchema *spec.Schema, resourceID string) (*spec.Schema, string, error) {
	typeName := krocel.TypeNamePrefix + resourceID
	currentSchema := rootSchema

	for _, seg := range segments {
		if seg.Name != "" {
			typeName = typeName + "." + seg.Name
			currentSchema = lookupSchemaAtField(c, currentSchema, seg.Name)
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

// getExpectedTypeForField computes the expected CEL type for a field descriptor
// by deriving it from the OpenAPI schema at the path.
func getExpectedTypeForField(builderCache *celcache.BuilderCache, schemaCache *schema.Cache, descriptor *variable.FieldDescriptor, rootSchema *spec.Schema, resourceID string, typeProvider *krocel.DeclTypeProvider) *cel.Type {
	segments, err := fieldpath.Parse(descriptor.Path)
	if err != nil {
		return cel.DynType
	}

	schema, typeName, err := resolveSchemaAndTypeName(schemaCache, segments, rootSchema, resourceID)
	if err != nil {
		return cel.DynType
	}

	return getCelTypeFromSchema(builderCache, schema, typeName, typeProvider)
}

// getCelTypeFromSchema looks up a pre-registered CEL type by name from the
// provider (O(1) hash lookup). Falls back to converting the schema directly
// for nested types that aren't registered at the top level.
func getCelTypeFromSchema(builderCache *celcache.BuilderCache, schema *spec.Schema, typeName string, typeProvider *krocel.DeclTypeProvider) *cel.Type {
	if typeProvider != nil {
		if declType, found := typeProvider.FindDeclType(typeName); found {
			return declType.CelType()
		}
	}

	// Fallback: convert schema directly for nested/leaf types not in the provider
	if schema == nil {
		return cel.DynType
	}
	schemaDeclType := func(s *spec.Schema) *apiservercel.DeclType {
		return krocel.SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: s}, false)
	}
	declType := builderCache.SchemaDeclType(schema, schemaDeclType)
	if declType == nil {
		return cel.DynType
	}
	declType = builderCache.MaybeAssignTypeName(schema, declType, typeName)
	return declType.CelType()
}

// lookupSchemaAtField resolves a single field name within a schema.
// Returns a pointer-stable result via the schema cache.
func lookupSchemaAtField(c *schema.Cache, s *spec.Schema, field string) *spec.Schema {
	if s == nil || field == "" {
		return s
	}

	if result := c.LookupField(s, field); result != nil {
		return result
	}

	if result := c.LookupAdditionalProperties(s); result != nil {
		return result
	}

	if s.Items != nil && s.Items.Schema != nil {
		return lookupSchemaAtField(c, s.Items.Schema, field)
	}

	return nil
}

// validateAndCompileNode validates and compiles all CEL expressions for a single node:
// - forEach expressions (collection iteration)
// - Template expressions (resource field values)
// - includeWhen expressions (conditional resource creation)
// - readyWhen expressions (resource readiness conditions)
//
// Uses the shared inspectorEnv for AST inspection and typed env for compilation.
func validateAndCompileNode(builderCache *celcache.BuilderCache, schemaCache *schema.Cache, sessionCache *celcache.SessionCache, node *Node, inspector *ast.Inspector, env *cel.Env, nodeSchema *spec.Schema, typeProvider *krocel.DeclTypeProvider) error {
	// Track iterator types for extending template environment
	var iteratorTypes map[string]*cel.Type

	// If this node has forEach iterators, validate and compile them
	if len(node.ForEach) > 0 {
		var err error
		iteratorTypes, err = validateAndCompileForEach(sessionCache, env, node, inspector)
		if err != nil {
			return err
		}
	}

	// Validate and compile template expressions
	if err := validateAndCompileTemplates(builderCache, schemaCache, sessionCache, env, node, nodeSchema, typeProvider, iteratorTypes); err != nil {
		return err
	}

	// Validate and compile includeWhen expressions if present
	if len(node.IncludeWhen) > 0 {
		// includeWhen expressions can reference schema plus any resource dependency
		// already discovered for this node. Resource refs are evaluated at runtime
		// against observed upstream state.
		allowedVars := append([]string{SchemaVarName}, node.Meta.Dependencies...)
		if err := validateConditionReferences(node.IncludeWhen, allowedVars); err != nil {
			return fmt.Errorf("resource %q includeWhen: %w", node.Meta.ID, err)
		}

		// Compile includeWhen using the shared typed environment
		if err := validateAndCompileIncludeWhen(sessionCache, env, node); err != nil {
			return err
		}
	}

	// Validate and compile readyWhen expressions if present
	if len(node.ReadyWhen) > 0 {
		// readyWhen expressions can ONLY reference the node itself (or 'each' for collections).
		// At runtime, IsResourceReady/IsCollectionReady only has the resource in scope.
		allowedVar := node.Meta.ID
		if node.Meta.Type == NodeTypeCollection {
			allowedVar = EachVarName
		}

		for _, expression := range node.ReadyWhen {
			if _, err := inspectExpressionRestricted(inspector, expression.Original, []string{allowedVar}); err != nil {
				return fmt.Errorf("resource %q readyWhen: %w", node.Meta.ID, err)
			}
		}

		// For readyWhen on collections, we need "each" variable which isn't in the shared env.
		// Extend the existing env with the node schema under "each" — uses env.Extend()
		// which reuses parent function bindings (cheaper than full env build), and caches
		// the result so 1000 identical collection nodes share one extended env.
		readyEnv := env
		if node.Meta.Type == NodeTypeCollection {
			var err error
			readyEnv, err = krocel.ExtendWithTypedVar(builderCache, sessionCache, env, EachVarName, nodeSchema)
			if err != nil {
				return fmt.Errorf("failed to create CEL environment for readyWhen validation: %w", err)
			}
		}

		if err := validateAndCompileReadyWhen(sessionCache, readyEnv, node); err != nil {
			return err
		}
	}

	return nil
}

// validateAndCompileTemplates validates and compiles CEL template expressions for a single node.
// For collections with forEach, the env is extended with iterator variable declarations.
func validateAndCompileTemplates(
	builderCache *celcache.BuilderCache,
	schemaCache *schema.Cache,
	sessionCache *celcache.SessionCache,
	env *cel.Env,
	node *Node,
	nodeSchema *spec.Schema,
	typeProvider *krocel.DeclTypeProvider,
	iteratorTypes map[string]*cel.Type,
) error {
	// NOTE: omit() is allowed in template expressions. Restricted-context
	// rejection (includeWhen, readyWhen, forEach) is handled by the compiler
	// in inspectExpressionRestricted and validateAndCompileForEach.
	// Identity-field rejection (metadata.name, metadata.namespace) is handled
	// by validateIdentityFields earlier in the build pipeline.
	compileEnv := env
	if len(iteratorTypes) > 0 {
		varsKey := celcache.MakeIteratorVarsKey(iteratorTypes)
		var err error
		compileEnv, err = sessionCache.ExtendWithVariables(env, varsKey, func() (*cel.Env, error) {
			opts := make([]cel.EnvOption, 0, len(iteratorTypes))
			for name, typ := range iteratorTypes {
				opts = append(opts, cel.Variable(name, typ))
			}
			return env.Extend(opts...)
		})
		if err != nil {
			return fmt.Errorf("failed to extend CEL environment with iterator types: %w", err)
		}
	}

	for _, templateVariable := range node.Variables {
		// Compute expected type for this field
		expectedType := getExpectedTypeForField(builderCache, schemaCache, &templateVariable.FieldDescriptor, nodeSchema, node.Meta.ID, typeProvider)

		expression := templateVariable.Expression
		displayExpr := expression.UserExpression()
		// Parse, type-check, and compile
		checkedAST, err := parseCheckAndCompile(sessionCache, compileEnv, expression)
		if err != nil {
			return fmt.Errorf("failed to compile template expression %q at path %q: %w", displayExpr, templateVariable.Path, err)
		}

		outputType := checkedAST.OutputType()
		if err := validateExpressionType(outputType, expectedType, displayExpr, node.Meta.ID, templateVariable.Path, typeProvider); err != nil {
			return err
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

// parseCheckAndCompile parses, type-checks, and compiles a CEL expression.
// On success, it sets expr.Program and returns the checked AST.
// Results are cached by (expression, environment) — programs and ASTs are
// immutable after construction and safe to share across goroutines.
// Callers should wrap errors with appropriate context.
func parseCheckAndCompile(sessionCache *celcache.SessionCache, env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	program, checkedAST, err := sessionCache.ParseCheckAndCompile(env, expr.Original)
	if err != nil {
		return nil, err
	}
	expr.Program = program

	return checkedAST, nil
}

// parseAndCheck only parses and type-checks a CEL expression (no program compilation).
// The checked AST is cached so that a later parseCheckAndCompile call for the same
// expression can skip the parse+check phases.
func parseAndCheck(sessionCache *celcache.SessionCache, env *cel.Env, expr *krocel.Expression) (*cel.Ast, error) {
	return sessionCache.ParseAndCheck(env, expr.Original)
}

// validateConditionExpression validates a single condition expression (includeWhen or readyWhen).
// It parses, type-checks, and verifies the expression returns bool or optional_type(bool).
func validateConditionExpression(sessionCache *celcache.SessionCache, env *cel.Env, expr *krocel.Expression, conditionType, resourceID string) error {
	checkedAST, err := parseCheckAndCompile(sessionCache, env, expr)
	if err != nil {
		return fmt.Errorf("failed to type-check %s expression %q in resource %q: %w", conditionType, expr.UserExpression(), resourceID, err)
	}

	// Verify the expression returns bool or optional_type(bool)
	outputType := checkedAST.OutputType()
	if !conversion.IsBoolOrOptionalBool(outputType) {
		return fmt.Errorf(
			"%s expression %q in resource %q must return bool or optional_type(bool), but returns %q",
			conditionType, expr.UserExpression(), resourceID, outputType.String(),
		)
	}

	return nil
}

// validateAndCompileIncludeWhen validates and compiles includeWhen expressions.
// These expressions must only reference the "schema" variable and return bool.
func validateAndCompileIncludeWhen(sessionCache *celcache.SessionCache, env *cel.Env, node *Node) error {
	for _, expression := range node.IncludeWhen {
		if err := validateConditionExpression(sessionCache, env, expression, "includeWhen", node.Meta.ID); err != nil {
			return err
		}
	}
	return nil
}

// validateAndCompileReadyWhen validates and compiles readyWhen expressions for a single node.
func validateAndCompileReadyWhen(sessionCache *celcache.SessionCache, env *cel.Env, node *Node) error {
	for _, expression := range node.ReadyWhen {
		if err := validateConditionExpression(sessionCache, env, expression, "readyWhen", node.Meta.ID); err != nil {
			return err
		}
	}
	return nil
}

// validateAndCompileForEach validates and compiles forEach expressions for a collection node.
// It returns a map of iterator variable names to their inferred CEL types.
//
// Each forEach expression must:
// 1. Be a valid CEL expression
// 2. Return a list type (the list will be iterated over)
//
// The inferred element type of each list is used to declare the iterator variable
// in the CEL environment for validating template expressions.
func validateAndCompileForEach(sessionCache *celcache.SessionCache, env *cel.Env, node *Node, inspector *ast.Inspector) (map[string]*cel.Type, error) {
	if len(node.ForEach) == 0 {
		return nil, nil
	}

	iteratorTypes := make(map[string]*cel.Type, len(node.ForEach))

	for _, iter := range node.ForEach {
		// Reject omit() in forEach expressions — it's only valid in resource
		// template expressions. We check the inspection result here to avoid
		// a redundant inspector.Inspect() call in a separate pass.
		inspection, inspErr := inspector.Inspect(iter.Expression.Original)
		if inspErr != nil {
			return nil, fmt.Errorf("node %q: forEach iterator %q: failed to inspect expression: %w", node.Meta.ID, iter.Name, inspErr)
		}
		if inspection.UsesOmit() {
			return nil, fmt.Errorf("node %q: forEach iterator %q: omit() can only be used in resource template expressions", node.Meta.ID, iter.Name)
		}

		// Parse, type-check, and compile the forEach expression
		checkedAST, err := parseCheckAndCompile(sessionCache, env, iter.Expression)
		if err != nil {
			return nil, fmt.Errorf("node %q: forEach iterator %q: %w", node.Meta.ID, iter.Name, err)
		}

		// Extract the element type from the list
		outputType := checkedAST.OutputType()
		elemType, err := krocel.ListElementType(outputType)
		if err != nil {
			return nil, fmt.Errorf("node %q: forEach iterator %q must return a list, got %q: %w",
				node.Meta.ID, iter.Name, outputType.String(), err)
		}

		iteratorTypes[iter.Name] = elemType
	}

	return iteratorTypes, nil
}

// getSchemaWithoutStatus extracts a spec.Schema from a CRD for CEL validation.
// It includes spec and metadata but excludes status, since status references
// are not allowed in RGD expressions. Cluster-scoped instance CRDs also omit
// metadata.namespace so CEL cannot type-check references to a field that does
// not exist at runtime.
func getSchemaWithoutStatus(crd *extv1.CustomResourceDefinition) (*spec.Schema, error) {
	if len(crd.Spec.Versions) != 1 {
		return nil, fmt.Errorf("expected CRD to have exactly one version, got %d versions", len(crd.Spec.Versions))
	}
	if crd.Spec.Versions[0].Schema == nil {
		return nil, fmt.Errorf("expected CRD version to have schema defined")
	}

	// Copy the schema and remove status
	openAPISchema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.DeepCopy()
	delete(openAPISchema.Properties, "status")

	specSchema, err := schema.ConvertJSONSchemaPropsToSpecSchema(openAPISchema)
	if err != nil {
		return nil, err
	}

	// Add full ObjectMeta schema for CEL validation
	if specSchema.Properties == nil {
		specSchema.Properties = make(map[string]spec.Schema)
	}
	metadataSchema := schema.ObjectMetaSchema
	if crd.Spec.Scope == extv1.ClusterScoped {
		metadataSchema = schema.NamespacelessObjectMetaSchema
	}
	specSchema.Properties["metadata"] = metadataSchema

	return specSchema, nil
}

// collectNodeSchemas builds a map of node IDs to their OpenAPI schemas.
// Collections (forEach) and external collections (selector) are wrapped as
// list types so other nodes can reference them as arrays and use CEL list functions.
func collectNodeSchemas(c *schema.Cache, nodes map[string]*Node, nodeSchemas map[string]*spec.Schema) map[string]*spec.Schema {
	result := make(map[string]*spec.Schema)
	for id, node := range nodes {
		if sch, ok := nodeSchemas[id]; ok {
			if node.Meta.Type == NodeTypeCollection || node.Meta.Type == NodeTypeExternalCollection {
				result[id] = c.WrapAsList(sch)
			} else {
				result[id] = sch
			}
		}
	}
	return result
}
