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
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/rest"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/cel/ast"
	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/cel/library"
	"github.com/kubernetes-sigs/kro/pkg/dag"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph/crd"
	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/parser"
	"github.com/kubernetes-sigs/kro/pkg/graph/schema"
	schemaresolver "github.com/kubernetes-sigs/kro/pkg/graph/schema/resolver"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

// NewBuilder creates a new Builder. By default it uses CombinedResolver for
// schema resolution and DynamicRESTMapper for resource discovery. Both can be
// overridden with BuilderOptions.
func NewBuilder(clientConfig *rest.Config, httpClient *http.Client, opts ...BuilderOption) (*Builder, error) {
	b := &Builder{}
	for _, opt := range opts {
		opt(b)
	}

	if b.schemaResolver == nil {
		sr, err := schemaresolver.NewCombinedResolver(clientConfig, httpClient)
		if err != nil {
			return nil, fmt.Errorf("failed to create schema resolver: %w", err)
		}
		b.schemaResolver = sr
	}

	if b.restMapper == nil {
		rm, err := apiutil.NewDynamicRESTMapper(clientConfig, httpClient)
		if err != nil {
			return nil, fmt.Errorf("failed to create dynamic REST mapper: %w", err)
		}
		b.restMapper = rm
	}

	return b, nil
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
	costLimit      uint64
}

// BuilderOption is an option for configuring a Builder.
type BuilderOption func(*Builder)

// WithSchemaResolver allows configuring a custom SchemaResolver for a Builder.
func WithSchemaResolver(r resolver.SchemaResolver) BuilderOption {
	return func(b *Builder) { b.schemaResolver = r }
}

// WithRESTMapper allows configuring a custom RESTMapper for a Builder.
func WithRESTMapper(rm meta.RESTMapper) BuilderOption {
	return func(b *Builder) { b.restMapper = rm }
}

// WithCostLimit allows configuring a custom CEL evaluation cost limit for a Builder.
// Setting costLimit to 0 disables cost limiting (default).
func WithCostLimit(limit uint64) BuilderOption {
	return func(b *Builder) { b.costLimit = limit }
}

// Config holds runtime configuration parameters shared by every graph consumer.
type Config struct {
	MaxCollectionSize          int
	MaxCollectionDimensionSize int
}

// NewResourceGraphDefinition creates a new ResourceGraphDefinition object from the given ResourceGraphDefinition
// CRD. The ResourceGraphDefinition object is a fully processed and validated representation
// of the resource graph definition CRD, it's underlying resources, and the relationships between
// the resources.
func (b *Builder) NewResourceGraphDefinition(originalCR *v1alpha1.ResourceGraphDefinition, rgdConfig Config) (*Graph, error) {
	// Copy so we never mutate the caller's object.
	rgd := originalCR.DeepCopy()

	// kro leverages CEL expressions, so resource/kind names must be valid CEL
	// identifiers (e.g. no "-", which CEL reads as subtraction).
	if err := validateResourceGraphDefinition(rgd, rgdConfig); err != nil {
		return nil, fmt.Errorf("failed to validate resourcegraphdefinition: %w", err)
	}

	// SimpleSchema -> instance spec schema -> synthesized CRD + status-stripped
	// CEL schema + scope. Depends only on the schema block, not resources, so it
	// is computed up front and fed to compileSource via the source.
	instanceCRD, schemaWithoutStatus, crdScope, err := synthesizeInstanceCRD(rgd)
	if err != nil {
		return nil, fmt.Errorf("failed to build resourcegraphdefinition %q: %w", rgd.Name, err)
	}

	resourceSpecs := make([]ResourceSpec, 0, len(rgd.Spec.Resources))
	for i, rgResource := range rgd.Spec.Resources {
		rs, err := b.rgResourceSpec(rgResource, i)
		if err != nil {
			return nil, fmt.Errorf("failed to build resource %q: %w", rgResource.ID, err)
		}
		resourceSpecs = append(resourceSpecs, rs)
	}

	g, statusSchema, err := b.compileSource(rgdSource{
		resources:       resourceSpecs,
		instanceGVR:     metadata.GetResourceGraphDefinitionInstanceGVR(rgd.Spec.Schema.Group, rgd.Spec.Schema.APIVersion, rgd.Spec.Schema.Kind),
		namespaced:      crdScope == extv1.NamespaceScoped,
		schemaVarSchema: schemaWithoutStatus,
		statusRaw:       rgd.Spec.Schema.Status.Raw,
	})
	if err != nil {
		return nil, err
	}

	crd.SetCRDStatus(instanceCRD, *statusSchema, true)
	g.CRD = instanceCRD
	return g, nil
}

// InstanceSchemaForCEL returns the OpenAPI schema bound to the `schema` CEL
// variable when compiling expressions for the given RGD: the instance spec
// converted from the RGD's SimpleSchema plus apiVersion/kind/metadata, with
// status stripped (status references are not allowed in RGD resource
// expressions).
//
// Exported so graph consumers can bind this as the type of the `schema` CEL
// variable, giving compile-time typing from the declared SimpleSchema rather
// than from a runtime instance value.
func InstanceSchemaForCEL(rgd *v1alpha1.ResourceGraphDefinition) (*spec.Schema, error) {
	_, schemaWithoutStatus, _, err := synthesizeInstanceCRD(rgd)
	return schemaWithoutStatus, err
}

// synthesizeInstanceCRD converts the RGD's SimpleSchema into the instance CRD
// (status placeholder empty, filled in later once inferred) and the
// status-stripped OpenAPI schema bound to the `schema` CEL variable, plus the
// CRD scope. Shared by NewResourceGraphDefinition and InstanceSchemaForCEL so
// the compile-time typing and the synthesized CRD stay in lock-step.
func synthesizeInstanceCRD(rgd *v1alpha1.ResourceGraphDefinition) (*extv1.CustomResourceDefinition, *spec.Schema, extv1.ResourceScope, error) {
	if rgd.Spec.Schema == nil {
		return nil, nil, "", fmt.Errorf("resourcegraphdefinition %q: schema is required", rgd.Name)
	}
	instanceSpecSchema, err := buildInstanceSpecSchema(rgd.Spec.Schema)
	if err != nil {
		return nil, nil, "", err
	}
	crdScope := extv1.NamespaceScoped
	if rgd.Spec.Schema.Scope == v1alpha1.ResourceScopeCluster {
		crdScope = extv1.ClusterScoped
	}
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
	schemaWithoutStatus, err := getSchemaWithoutStatus(instanceCRD)
	if err != nil {
		return nil, nil, "", err
	}
	return instanceCRD, schemaWithoutStatus, crdScope, nil
}

// rgdSource adapts a ResourceGraphDefinition's precomputed pieces to source.
type rgdSource struct {
	resources       []ResourceSpec
	instanceGVR     k8sschema.GroupVersionResource
	namespaced      bool
	schemaVarSchema *spec.Schema
	statusRaw       []byte
}

func (s rgdSource) Resources() []ResourceSpec                   { return s.resources }
func (s rgdSource) InstanceGVR() k8sschema.GroupVersionResource { return s.instanceGVR }
func (s rgdSource) InstanceNamespaced() bool                    { return s.namespaced }
func (s rgdSource) SchemaVarSchema() *spec.Schema               { return s.schemaVarSchema }
func (s rgdSource) StatusRaw() []byte                           { return s.statusRaw }

// compileSource compiles a source into a Graph: it builds resource nodes, derives
// the dependency DAG, type-checks every CEL expression against target schemas,
// infers the instance status schema, and assembles the instance node. It does not
// synthesize a CRD; callers that need one attach it to the returned Graph using
// the returned status schema. This is the shared entry point for any graph consumer.
func (b *Builder) compileSource(src source) (*Graph, *extv1.JSONSchemaProps, error) {
	// Per-build schema cache: pointer-stable field lookups, discarded after build.
	schemaCache := schema.NewCache()
	p := parser.New(schemaCache)

	nodes := make(map[string]*Node)
	schemas := make(map[string]*spec.Schema)
	for _, rs := range src.Resources() {
		node, nodeSchema, err := b.buildResourceNode(p, rs, src.InstanceNamespaced())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to build resource %q: %w", rs.ID, err)
		}
		if nodes[rs.ID] != nil {
			return nil, nil, fmt.Errorf("found resources with duplicate id %q", rs.ID)
		}
		nodes[rs.ID] = node
		schemas[rs.ID] = nodeSchema
	}

	// Lightweight inspector env: declares identifier names only (enough to parse
	// and find references, not to type-check or compile).
	nodeNames := make([]string, 0, len(nodes))
	for name := range nodes {
		nodeNames = append(nodeNames, name)
	}
	allIdentifiers := slices.Concat(nodeNames, []string{SchemaVarName, EachVarName, library.RuntimeVarName})
	inspectorEnv, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(allIdentifiers))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create inspector environment: %w", err)
	}
	inspector := ast.NewInspectorWithEnv(inspectorEnv, allIdentifiers)

	// Dependency graph first, so undeclared-resource errors surface clearly
	// rather than as downstream CEL type errors.
	dag, err := b.buildDependencyGraph(nodes, inspector)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}
	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get topological order: %w", err)
	}
	applyOrders, err := applyOrdersForDAG(dag)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get apply-order waves: %w", err)
	}

	if features.FeatureGate.Enabled(features.CELOmitFunction) {
		if err := validateIdentityFields(nodes, inspector, src.InstanceNamespaced()); err != nil {
			return nil, nil, err
		}
	}

	celSchemas := collectNodeSchemas(schemaCache, nodes, schemas)
	celSchemas[SchemaVarName] = src.SchemaVarSchema()

	typedEnv, typeProvider, err := krocel.TypedEnvironmentWithProvider(celSchemas)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create typed CEL environment: %w", err)
	}

	bc := &buildContext{
		env:          typedEnv,
		typeProvider: typeProvider,
		schemaCache:  schemaCache,
		declTypes:    make(map[*spec.Schema]*apiservercel.DeclType),
		checkedASTs:  make(map[checkedASTKey]*cel.Ast),
		extendedEnvs: make(map[extendedEnvKey]*cel.Env),
		costLimit:    b.costLimit,
	}

	for id, node := range nodes {
		if err := validateAndCompileNode(bc, node, inspector, schemas[id]); err != nil {
			return nil, nil, fmt.Errorf("failed to validate resource %q: %w", id, err)
		}
	}

	unstructuredStatus := map[string]any{}
	if err := yaml.UnmarshalStrict(src.StatusRaw(), &unstructuredStatus); err != nil {
		return nil, nil, fmt.Errorf("failed to unmarshal status schema: %w", err)
	}
	statusSchema, statusVariables, statusTemplate, conditionExprStrings, err := inferStatusSchema(bc, unstructuredStatus, nodeNames, inspector)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build instance status schema: %w", err)
	}

	for _, fd := range statusVariables {
		if _, err := bc.compile(bc.env, fd.Expression); err != nil {
			return nil, nil, fmt.Errorf("failed to compile status expression %q at path %q: %w", fd.Expression.UserExpression(), fd.Path, err)
		}
	}

	conditions, err := buildConditions(bc, conditionExprStrings, inspector, inspectorEnv, nodeNames)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to build instance conditions: %w", err)
	}

	instance, err := buildInstanceNode(
		src.InstanceGVR(),
		src.InstanceNamespaced(),
		statusVariables,
		statusTemplate,
		conditions,
		inspector,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create instance node: %w", err)
	}

	resourceSchemas := make(map[string]*spec.Schema, len(schemas)+1)
	maps.Copy(resourceSchemas, schemas)
	resourceSchemas[InstanceNodeID] = src.SchemaVarSchema()

	g := &Graph{
		DAG:              dag,
		Instance:         instance,
		Nodes:            nodes,
		Resources:        nodes,
		TopologicalOrder: topologicalOrder,
		ApplyOrders:      applyOrders,
		ResourceSchemas:  resourceSchemas,
	}
	return g, statusSchema, nil
}

// buildExternalRefResource builds an empty resource with metadata from the given externalRef definition.
// The selector (if any) is embedded directly in the template so that ParseSchemalessResource
// can extract CEL expressions from the entire resource in a single pass.
func (b *Builder) buildExternalRefResource(
	externalRef *v1alpha1.ExternalRef) (map[string]any, error) {
	result, err := runtime.DefaultUnstructuredConverter.ToUnstructured(externalRef)
	if err != nil {
		return nil, fmt.Errorf("failed to convert ExternalRef to unstructured: %w", err)
	}
	return result, nil
}

// rgResourceSpec projects an RGD resource into a schema-agnostic ResourceSpec:
// it validates field combinations and resolves the template object (user YAML or
// external ref serialized to unstructured).
func (b *Builder) rgResourceSpec(rgResource *v1alpha1.Resource, order int) (ResourceSpec, error) {
	if err := validateCombinableResourceFields(rgResource.ID, len(rgResource.Template.Raw) > 0, rgResource.ExternalRef != nil, len(rgResource.ForEach)); err != nil {
		return ResourceSpec{}, fmt.Errorf("invalid combination of resource fields: %w", err)
	}
	if rgResource.ExternalRef != nil {
		if err := validateExternalRefMetadata(rgResource.ExternalRef.Metadata); err != nil {
			return ResourceSpec{}, fmt.Errorf("invalid external ref metadata for resource %s: %w", rgResource.ID, err)
		}
	}

	resourceObject := map[string]any{}
	if len(rgResource.Template.Raw) > 0 {
		if err := yaml.UnmarshalStrict(rgResource.Template.Raw, &resourceObject); err != nil {
			return ResourceSpec{}, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.ID, err)
		}
	} else {
		var err error
		if resourceObject, err = b.buildExternalRefResource(rgResource.ExternalRef); err != nil {
			return ResourceSpec{}, fmt.Errorf("failed to build external ref resource %s: %w", rgResource.ID, err)
		}
	}

	forEach := make([]map[string]string, len(rgResource.ForEach))
	for i, d := range rgResource.ForEach {
		forEach[i] = d
	}

	return ResourceSpec{
		ID:          rgResource.ID,
		Object:      resourceObject,
		ExternalRef: rgResource.ExternalRef != nil,
		Collection:  rgResource.ExternalRef != nil && rgResource.ExternalRef.Metadata.HasSelector(),
		ReadyWhen:   rgResource.ReadyWhen,
		IncludeWhen: rgResource.IncludeWhen,
		ForEach:     forEach,
		Order:       order,
	}, nil
}

// buildResourceNode builds a node from a schema-agnostic ResourceSpec: it
// resolves the GVK/schema/REST mapping, validates template constraints, extracts
// CEL field descriptors, and assembles the Node. Returns the Node and the
// OpenAPI schema (used during build for CEL validation only).
func (b *Builder) buildResourceNode(
	p *parser.Parser,
	rs ResourceSpec,
	instanceNamespaced bool,
) (*Node, *spec.Schema, error) {
	if err := validateKubernetesObjectStructure(rs.Object); err != nil {
		return nil, nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rs.ID, err)
	}

	gvk, err := metadata.ExtractGVKFromUnstructured(rs.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rs.ID, err)
	}

	resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get schema for resource %s: %w", rs.ID, err)
	}

	mapping, err := b.restMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get REST mapping for resource %s: %w", rs.ID, err)
	}
	if err := validateTemplateConstraints(
		rs.ID,
		rs.Collection,
		rs.Object,
		mapping.Scope.Name() == meta.RESTScopeNameNamespace,
		instanceNamespaced,
	); err != nil {
		return nil, nil, err
	}

	// Extract CEL fieldDescriptors from the resource.
	var fieldDescriptors []variable.FieldDescriptor
	if rs.ExternalRef {
		// External ref templates are synthetic (not user YAML), so use the
		// schemaless parser for the entire resource uniformly.
		fieldDescriptors, _, err = parser.ParseSchemalessResource(rs.Object)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse external ref resource %s: %w", rs.ID, err)
		}
	} else if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		fieldDescriptors, _, err = parser.ParseSchemalessResource(rs.Object)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rs.ID, err)
		}

		for _, expr := range fieldDescriptors {
			if !strings.HasPrefix(expr.Path, "metadata.") {
				return nil, nil, fmt.Errorf("CEL expressions in CRDs are only supported for metadata fields, found in path %q, resource %s", expr.Path, rs.ID)
			}
		}
	} else {
		fieldDescriptors, err = p.ParseResource(rs.Object, resourceSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rs.ID, err)
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

	readyWhen, err := parser.UnwrapExpressions(rs.ReadyWhen)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse readyWhen expressions: %v", err)
	}

	includeWhen, err := parser.UnwrapExpressions(rs.IncludeWhen)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse includeWhen expressions: %v", err)
	}

	forEachDimensions, err := parseForEachDimensions(rs.ForEach)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse forEach dimensions: %v", err)
	}

	// Determine node type.
	nodeType := NodeTypeResource
	if rs.ExternalRef {
		if rs.Collection {
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
			ID:         rs.ID,
			Index:      rs.Order,
			Type:       nodeType,
			GVR:        mapping.Resource,
			Namespaced: mapping.Scope.Name() == meta.RESTScopeNameNamespace,
			// Dependencies will be set by buildDependencyGraph
		},
		Template:    &unstructured.Unstructured{Object: rs.Object},
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

		// Build deduplicated dependency list, then register with the DAG.
		for _, dep := range templateDeps {
			node.Meta.addDependency(dep)
		}
		for _, dep := range forEachDeps {
			node.Meta.addDependency(dep)
		}
		for _, dep := range includeWhenDeps {
			node.Meta.addDependency(dep)
		}
		if err := directedAcyclicGraph.AddDependencies(node.Meta.ID, node.Meta.Dependencies); err != nil {
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
	gvr k8sschema.GroupVersionResource,
	namespaced bool,
	statusVariables []variable.FieldDescriptor,
	statusTemplate map[string]any,
	conditions []*krocel.Expression,
	inspector *ast.Inspector,
) (*Node, error) {

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
		// Status fields may reference resources, schema, or both.
		referencesSchema := slices.Contains(statusVariable.Expression.References, SchemaVarName)
		if len(deps) == 0 && !referencesSchema {
			return nil, fmt.Errorf("instance status field must refer to a resource or schema: %s", statusVariable.Path)
		}
		instanceDeps = append(instanceDeps, deps...)
		// If this expression references schema, include the instance node itself as a dep
		// so the runtime wires it into instNode.deps for context building.
		if referencesSchema && !slices.Contains(instanceDeps, InstanceNodeID) {
			instanceDeps = append(instanceDeps, InstanceNodeID)
		}

		instanceStatusVariables = append(instanceStatusVariables, &variable.ResourceField{
			FieldDescriptor: statusVariable,
			Kind:            variable.ResourceVariableKindDynamic,
		})
	}

	// Fold condition dependencies into instanceDeps so the instance reconciles
	// after the resources its conditions read. References were populated by
	// buildConditions; schema and runtime are not resource dependencies.
	for _, expr := range conditions {
		for _, ref := range expr.References {
			if ref == SchemaVarName || ref == library.RuntimeVarName {
				continue
			}
			if !slices.Contains(instanceDeps, ref) {
				instanceDeps = append(instanceDeps, ref)
			}
		}
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
			Object: map[string]any{
				"status": statusTemplate,
			},
		},
		Variables:  instanceStatusVariables,
		Conditions: conditions,
	}

	return instance, nil
}

// BuildInstanceSpecSchema converts an RGD Schema's SimpleSchema spec (and
// optional custom types) into the OpenAPI JSONSchemaProps used to synthesize
// the instance CRD. Exported so graph consumers can reuse the same
// SimpleSchema → OpenAPI conversion without depending on unexported builder
// internals.
func BuildInstanceSpecSchema(rgSchema *v1alpha1.Schema) (*extv1.JSONSchemaProps, error) {
	return buildInstanceSpecSchema(rgSchema)
}

// buildInstanceSpecSchema builds the instance spec schema that will be
// used to generate the CRD for the instance resource. The instance spec
// schema is expected to be defined using the "SimpleSchema" format.
func buildInstanceSpecSchema(rgSchema *v1alpha1.Schema) (*extv1.JSONSchemaProps, error) {
	// We need to unmarshal the instance schema to a map[string]interface{} to
	// make it easier to work with.
	instanceSpec := map[string]any{}
	err := yaml.UnmarshalStrict(rgSchema.Spec.Raw, &instanceSpec)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal spec schema: %w", err)
	}

	// Also the custom types must be unmarshalled to a map[string]interface{} to
	// make handling easier.
	customTypes := map[string]any{}
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
// using CEL type checking. Uses the shared inspectorEnv for validation and
// typed env for compilation.
//
// Returns: (schema, fieldDescriptors, statusTemplate, conditionExprs, error)
func buildStatusSchema(
	bc *buildContext,
	rgSchema *v1alpha1.Schema,
	nodeNames []string,
	inspector *ast.Inspector,
) (
	*extv1.JSONSchemaProps,
	[]variable.FieldDescriptor,
	map[string]any,
	[]string,
	error,
) {
	// The instance resource has a schema defined using the "SimpleSchema" format.
	unstructuredStatus := map[string]any{}
	err := yaml.UnmarshalStrict(rgSchema.Status.Raw, &unstructuredStatus)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to unmarshal status schema: %w", err)
	}
	return inferStatusSchema(bc, unstructuredStatus, nodeNames, inspector)
}

// inferStatusSchema infers the instance status schema from CEL expressions in a
// pre-unmarshalled status map. Author-defined conditions are extracted first,
// remaining fields are type-checked and converted to an OpenAPI schema.
// Returns: (schema, fieldDescriptors, statusTemplate, conditionExprs, error).
func inferStatusSchema(
	bc *buildContext,
	unstructuredStatus map[string]any,
	nodeNames []string,
	inspector *ast.Inspector,
) (
	*extv1.JSONSchemaProps,
	[]variable.FieldDescriptor,
	map[string]any,
	[]string,
	error,
) {
	// Extract author-defined conditions before running CEL inference: the
	// `conditions:` expressions return Condition values, so inferring their
	// types would produce a wrong CRD schema. Removing the key leaves the
	// inference path unchanged for other fields, and crd.SetCRDStatus injects
	// the standard []metav1.Condition schema.
	conditionExprs, err := extractConditionExpressions(unstructuredStatus)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to extract conditions block: %w", err)
	}

	// Extract CEL expressions from the status field.
	fieldDescriptors, noExpressionFields, err := parser.ParseSchemalessResource(unstructuredStatus)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
	}

	if len(noExpressionFields) > 0 {
		return nil, nil, nil, nil, fmt.Errorf("status fields without expressions are not supported: %v", noExpressionFields)
	}

	// Verify status expressions only reference known resources or schema, and populate References.
	allowedStatusVars := slices.Concat(nodeNames, []string{SchemaVarName})
	for _, fieldDescriptor := range fieldDescriptors {
		expression := fieldDescriptor.Expression
		result, err := inspectExpressionRestricted(inspector, expression.Original, allowedStatusVars)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("status field %q expression %q: %w", fieldDescriptor.Path, expression.UserExpression(), err)
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
	// in a separate pass after buildStatusSchema returns.
	statusTypeMap := make(map[string]*cel.Type)
	for _, fieldDescriptor := range fieldDescriptors {
		expression := fieldDescriptor.Expression

		checkedAST, err := bc.parseAndCheck(bc.env, expression)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to type-check status expression %q at path %q: %w", expression.UserExpression(), fieldDescriptor.Path, err)
		}

		statusTypeMap[fieldDescriptor.Path] = checkedAST.OutputType()
	}

	// convert the CEL types to OpenAPI schema - best effort.
	statusSchema, err := schema.GenerateSchemaFromCELTypes(statusTypeMap, bc.typeProvider)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("failed to generate status schema from CEL types: %w", err)
	}

	return statusSchema, fieldDescriptors, unstructuredStatus, conditionExprs, nil
}

// extractConditionExpressions removes the `conditions:` key from the raw
// status YAML map and returns its values as expression strings.
//
// The `conditions:` block must be a list whose elements are CEL expression
// strings (each wrapped in `${...}`). Anything else is rejected.
//
// If the key is absent, returns (nil, nil).
func extractConditionExpressions(unstructuredStatus map[string]any) ([]string, error) {
	const conditionsKey = "conditions"

	raw, ok := unstructuredStatus[conditionsKey]
	if !ok {
		return nil, nil
	}
	delete(unstructuredStatus, conditionsKey)

	rawList, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("status.conditions must be a list, got %T", raw)
	}

	exprs := make([]string, 0, len(rawList))
	for i, elem := range rawList {
		s, ok := elem.(string)
		if !ok {
			return nil, fmt.Errorf("status.conditions[%d] must be a CEL expression string, got %T", i, elem)
		}
		exprs = append(exprs, s)
	}
	return exprs, nil
}

// buildConditions parses, validates, type-checks, and compiles the author's
// condition expressions, preserving input order. Returns nil when there are
// no conditions.
func buildConditions(
	bc *buildContext,
	conditionExprStrings []string,
	inspector *ast.Inspector,
	inspectorEnv *cel.Env,
	nodeNames []string,
) ([]*krocel.Expression, error) {
	if len(conditionExprStrings) == 0 {
		return nil, nil
	}

	// Strip the ${...} wrappers; References and Program are filled in below.
	conditions, err := parser.UnwrapExpressions(conditionExprStrings)
	if err != nil {
		return nil, fmt.Errorf("invalid conditions block: %w", err)
	}

	// Enforce the self-reference and literal-value rules. The structural
	// rules for runtime.newCondition are handled by its parse-time macro.
	stripped := make([]string, len(conditions))
	for i, c := range conditions {
		stripped[i] = c.Original
	}
	if err := validateConditionExpressions(inspectorEnv, stripped); err != nil {
		return nil, err
	}

	// Record each expression's references (resources, schema, runtime) so the
	// runtime keeps them in the eval activation.
	allowedRefs := append(slices.Clone(nodeNames), SchemaVarName, library.RuntimeVarName)
	for _, expr := range conditions {
		result, err := inspectExpressionRestricted(inspector, expr.Original, allowedRefs)
		if err != nil {
			return nil, fmt.Errorf("condition %q: %w", expr.UserExpression(), err)
		}
		for _, dep := range result.ResourceDependencies {
			if !slices.Contains(expr.References, dep.ID) {
				expr.References = append(expr.References, dep.ID)
			}
		}
	}

	for _, expr := range conditions {
		checkedAST, err := bc.parseAndCheck(bc.env, expr)
		if err != nil {
			return nil, fmt.Errorf("failed to type-check condition %q: %w", expr.UserExpression(), err)
		}
		if err := validateConditionOutputType(checkedAST.OutputType()); err != nil {
			return nil, fmt.Errorf("condition %q: %w", expr.UserExpression(), err)
		}
		if _, err := bc.compile(bc.env, expr); err != nil {
			return nil, fmt.Errorf("failed to compile condition %q: %w", expr.UserExpression(), err)
		}
	}

	return conditions, nil
}

// validateConditionOutputType verifies that a condition expression returns a
// Condition or a list of Conditions. Dyn is tolerated (e.g. mapping over an
// untyped collection); the runtime rejects malformed results in that case.
func validateConditionOutputType(outputType *cel.Type) error {
	t := outputType
	if elem, err := krocel.ListElementType(t); err == nil {
		t = elem
	}
	if library.IsConditionType(t) || t.IsExactType(cel.DynType) {
		return nil
	}
	return fmt.Errorf(
		"condition expressions must return runtime.newCondition(...) or a list of them, got %q",
		outputType.String(),
	)
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
		// SchemaVarName is the instance spec, not a resource dependency.
		if resource.ID == SchemaVarName {
			continue
		}
		// The runtime library variable is only injected when evaluating
		// status.conditions expressions (inspected in buildConditions, not
		// here); reject it everywhere else.
		if resource.ID == library.RuntimeVarName {
			return nil, nil, fmt.Errorf("runtime is only available in status.conditions expressions")
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
		inspectionResult, err := inspector.Inspect(expression.Original)
		if err != nil {
			return nil, fmt.Errorf("failed to inspect expression: %w", err)
		}
		if inspectionResult.UsesOmit() {
			return nil, fmt.Errorf("omit() can only be used in resource template expressions")
		}

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

// parseForEachDimensions converts forEach dimensions (single-entry {name: expr}
// maps) to ForEachDimension structs.
func parseForEachDimensions(apiDimensions []map[string]string) ([]ForEachDimension, error) {
	if len(apiDimensions) == 0 {
		return nil, nil
	}

	result := make([]ForEachDimension, 0, len(apiDimensions))
	for _, dimensionMap := range apiDimensions {
		// Each dimension is a map with exactly one entry
		for name, expression := range dimensionMap {
			// Parse the expression to extract the raw CEL (strip ${...} wrapper if present)
			parsedExprs, err := parser.UnwrapExpressions([]string{expression})
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

// expectedTypeForField computes the expected CEL type for a field descriptor
// by deriving it from the OpenAPI schema at the path.
func expectedTypeForField(bc *buildContext, descriptor *variable.FieldDescriptor, rootSchema *spec.Schema, resourceID string, nodeType NodeType) *cel.Type {
	// Paths under metadata.selector come from ExternalRef synthetic resources.
	// The selector is always a LabelSelector, whose structure is known, but
	// it doesn't exist in the target resource's OpenAPI schema. Return the
	// concrete types so the CEL type checker can catch mismatches. Other node
	// types keep schema-derived typing: a schemaless resource may legitimately
	// carry an unrelated field at metadata.selector.
	if nodeType == NodeTypeExternalCollection {
		if t := selectorFieldType(descriptor.Path); t != nil {
			return t
		}
	}

	segments, err := fieldpath.Parse(descriptor.Path)
	if err != nil {
		return cel.DynType
	}

	s, typeName, err := resolveSchemaAndTypeName(bc.schemaCache, segments, rootSchema, resourceID)
	if err != nil {
		return cel.DynType
	}

	return celTypeFromSchema(bc, s, typeName)
}

// selectorFieldType returns the expected CEL type for well-known
// LabelSelector fields under metadata.selector. Returns nil for paths
// that are not part of the selector structure.
//
// These types are intentionally lenient (dyn-valued maps/lists) so that a
// standalone expression like `selector: ${schema.spec.selector}` type-checks
// against a loosely-typed user schema field. Structural validation of the
// selector is handled by validateSelector in pkg/graph/validation.go.
func selectorFieldType(path string) *cel.Type {
	switch {
	case path == "metadata.selector":
		return cel.MapType(cel.StringType, cel.DynType)
	case path == "metadata.selector.matchLabels":
		return cel.MapType(cel.StringType, cel.StringType)
	case strings.HasPrefix(path, "metadata.selector.matchLabels."):
		return cel.StringType
	case path == "metadata.selector.matchExpressions":
		return cel.ListType(cel.DynType)
	default:
		return nil
	}
}

// celTypeFromSchema looks up a pre-registered CEL type by name from the
// provider (O(1) hash lookup). Falls back to converting the schema directly
// for nested types that aren't registered at the top level, using the
// buildContext's memoized DeclType cache.
func celTypeFromSchema(bc *buildContext, s *spec.Schema, typeName string) *cel.Type {
	if bc.typeProvider != nil {
		if declType, found := bc.typeProvider.FindDeclType(typeName); found {
			return declType.CelType()
		}
	}

	// Fallback: convert schema directly for nested/leaf types not in the provider
	declType := bc.schemaDeclType(s)
	if declType == nil {
		return cel.DynType
	}
	declType = declType.MaybeAssignTypeName(typeName)
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
func validateAndCompileNode(bc *buildContext, node *Node, inspector *ast.Inspector, nodeSchema *spec.Schema) error {
	// Track iterator types for extending template environment
	var iteratorTypes map[string]*cel.Type

	// If this node has forEach iterators, validate and compile them
	if len(node.ForEach) > 0 {
		var err error
		iteratorTypes, err = validateAndCompileForEach(bc, node, inspector)
		if err != nil {
			return err
		}
	}

	// Validate and compile template expressions
	if err := validateAndCompileTemplates(bc, node, nodeSchema, iteratorTypes); err != nil {
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
		if err := validateAndCompileIncludeWhen(bc, node); err != nil {
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
		// Uses cached extended env so N identical collection nodes share one env.
		readyEnv := bc.env
		if node.Meta.Type == NodeTypeCollection {
			var err error
			readyEnv, err = bc.extendWithTypedVar(bc.env, EachVarName, nodeSchema)
			if err != nil {
				return fmt.Errorf("failed to create CEL environment for readyWhen validation: %w", err)
			}
		}

		if err := validateAndCompileReadyWhen(bc, readyEnv, node); err != nil {
			return err
		}
	}

	return nil
}

// validateAndCompileTemplates validates and compiles CEL template expressions for a single node.
// For collections with forEach, the env is extended with iterator variable declarations.
func validateAndCompileTemplates(
	bc *buildContext,
	node *Node,
	nodeSchema *spec.Schema,
	iteratorTypes map[string]*cel.Type,
) error {
	// NOTE: omit() is allowed in template expressions. Restricted-context
	// rejection (includeWhen, readyWhen, forEach) is handled by the compiler
	// in inspectExpressionRestricted and validateAndCompileForEach.
	// Identity-field rejection (metadata.name, metadata.namespace) is handled
	// by validateIdentityFields earlier in the build pipeline.
	compileEnv := bc.env
	if len(iteratorTypes) > 0 {
		opts := make([]cel.EnvOption, 0, len(iteratorTypes))
		for name, typ := range iteratorTypes {
			opts = append(opts, cel.Variable(name, typ))
		}
		var err error
		compileEnv, err = bc.env.Extend(opts...)
		if err != nil {
			return fmt.Errorf("failed to extend CEL environment with iterator types: %w", err)
		}
	}

	for _, templateVariable := range node.Variables {
		// Compute expected type for this field
		expectedType := expectedTypeForField(bc, &templateVariable.FieldDescriptor, nodeSchema, node.Meta.ID, node.Meta.Type)

		expression := templateVariable.Expression
		displayExpr := expression.UserExpression()
		// Parse, type-check, and compile
		checkedAST, err := bc.compile(compileEnv, expression)
		if err != nil {
			return fmt.Errorf("failed to compile template expression %q at path %q: %w", displayExpr, templateVariable.Path, err)
		}

		outputType := checkedAST.OutputType()
		if err := validateExpressionType(outputType, expectedType, displayExpr, node.Meta.ID, templateVariable.Path, bc.typeProvider); err != nil {
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

// validateConditionExpression validates a single condition expression (includeWhen or readyWhen).
// It parses, type-checks, and verifies the expression returns bool or optional_type(bool).
func validateConditionExpression(bc *buildContext, env *cel.Env, expr *krocel.Expression, conditionType, resourceID string) error {
	checkedAST, err := bc.compile(env, expr)
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
func validateAndCompileIncludeWhen(bc *buildContext, node *Node) error {
	for _, expression := range node.IncludeWhen {
		if err := validateConditionExpression(bc, bc.env, expression, "includeWhen", node.Meta.ID); err != nil {
			return err
		}
	}
	return nil
}

// validateAndCompileReadyWhen validates and compiles readyWhen expressions for a single node.
func validateAndCompileReadyWhen(bc *buildContext, readyEnv *cel.Env, node *Node) error {
	for _, expression := range node.ReadyWhen {
		if err := validateConditionExpression(bc, readyEnv, expression, "readyWhen", node.Meta.ID); err != nil {
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
func validateAndCompileForEach(bc *buildContext, node *Node, inspector *ast.Inspector) (map[string]*cel.Type, error) {
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
		checkedAST, err := bc.compile(bc.env, iter.Expression)
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
func getSchemaWithoutStatus(crd *extv1.CustomResourceDefinition) (*spec.Schema, error) {
	if len(crd.Spec.Versions) != 1 {
		return nil, fmt.Errorf("expected CRD to have exactly one version, got %d versions", len(crd.Spec.Versions))
	}
	if crd.Spec.Versions[0].Schema == nil {
		return nil, fmt.Errorf("expected CRD version to have schema defined")
	}
	return stripStatusFromSchema(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, crd.Spec.Scope == extv1.ClusterScoped)
}

// stripStatusFromSchema converts an instance OpenAPI schema to a spec.Schema for
// CEL validation: status is dropped (status references are not allowed in RGD
// expressions) and a full ObjectMeta schema is added. Cluster-scoped instances
// omit metadata.namespace so CEL cannot type-check a field absent at runtime.
// The input is not mutated.
func stripStatusFromSchema(openAPI *extv1.JSONSchemaProps, isClusterScoped bool) (*spec.Schema, error) {
	openAPISchema := openAPI.DeepCopy()
	delete(openAPISchema.Properties, "status")

	specSchema, err := schema.ConvertJSONSchemaPropsToSpecSchema(openAPISchema)
	if err != nil {
		return nil, err
	}

	if specSchema.Properties == nil {
		specSchema.Properties = make(map[string]spec.Schema)
	}
	metadataSchema := schema.ObjectMetaSchema
	if isClusterScoped {
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
