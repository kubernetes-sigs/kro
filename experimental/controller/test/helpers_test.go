package graphcontroller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	graphcontroller "github.com/kubernetes-sigs/kro/experimental/controller"
	"github.com/kubernetes-sigs/kro/experimental/deploy"
)

// GraphGVK is a local alias for the exported controller GVK.
var GraphGVK = graphcontroller.GraphGVK

// GraphRevisionGVK is a local alias for the exported revision GVK.
var GraphRevisionGVK = graphcontroller.GraphRevisionGVK

// --- Helpers ---

// loadEmbeddedCRD reads a CRD from the embedded deploy.CRDs filesystem.
// Single source of truth: the same YAMLs used by bootstrap mode.
func loadEmbeddedCRD(filename string) *apiextensionsv1.CustomResourceDefinition {
	data, err := deploy.CRDs.ReadFile(filename)
	if err != nil {
		panic("reading embedded CRD " + filename + ": " + err.Error())
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(data, crd); err != nil {
		panic("unmarshalling embedded CRD " + filename + ": " + err.Error())
	}
	return crd
}

func buildGraphCRD() *apiextensionsv1.CustomResourceDefinition {
	return loadEmbeddedCRD("experimental.kro.run_graphs.yaml")
}

func buildGraphRevisionCRD() *apiextensionsv1.CustomResourceDefinition {
	return loadEmbeddedCRD("experimental.kro.run_graphrevisions.yaml")
}

func waitForCRD(ctx context.Context, c client.Client, name string) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := c.Get(ctx, types.NamespacedName{Name: name}, crd); err != nil {
			return false, nil
		}
		for _, cond := range crd.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
}

// buildRGDControllerGraph returns the L0 Graph that implements the RGD controller.
// This is the example-3 pattern: watch RGDs → stamp L1 Graphs → each L1 creates
// CRD + watches instances + stamps L2 Graphs.
func buildRGDControllerGraph(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rgd-controller",
				"namespace": namespace,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "rgds",
						"template": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "ResourceGraphDefinition",
							"selector":   map[string]any{},
						},
					},
					map[string]any{
						"id": "controllers",
						"forEach": map[string]any{
							"rgd": "${rgds}",
						},
						"template": map[string]any{
							"apiVersion": "experimental.kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								"name": "${rgd.metadata.name}-controller",
							},
							"spec": map[string]any{
								"nodes": `${[
									{"id": "rgd", "template": {
										"apiVersion": "test.kro.run/v1alpha1",
										"kind": "ResourceGraphDefinition",
										"metadata": {"name": rgd.metadata.name, "namespace": rgd.metadata.namespace}
									}},
									{"id": "crd", "template": {
										"apiVersion": "apiextensions.k8s.io/v1",
										"kind": "CustomResourceDefinition",
										"metadata": {
											"name": "${plural(rgd.spec.schema.kind).lowerAscii()}.${rgd.spec.schema.group}"
										},
										"spec": {
											"group": "${rgd.spec.schema.group}",
											"names": {
												"kind": "${rgd.spec.schema.kind}",
												"plural": "${plural(rgd.spec.schema.kind).lowerAscii()}"
											},
											"scope": "Namespaced",
											"versions": [{
												"name": "${rgd.spec.schema.apiVersion}",
												"served": true,
												"storage": true,
												"subresources": {"status": {}},
												"schema": {
													"openAPIV3Schema": "${simpleSchema.toOpenAPI(rgd.spec.schema, rgd.spec.nodes)}"
												}
											}]
										}
									}},
									{"id": "instances", "template": {
										"apiVersion": "${rgd.spec.schema.group}/${rgd.spec.schema.apiVersion}",
										"kind": "${rgd.spec.schema.kind}",
										"selector": {}
									}},
									{"id": "instanceGraphs", "forEach": {"instance": "${instances}"},
									 "template": {
										"apiVersion": "experimental.kro.run/v1alpha1",
										"kind": "Graph",
										"metadata": {"name": "${instance.metadata.name}-${rgd.spec.schema.kind.lowerAscii()}"},
										"spec": {
											"nodes": "${" +
												"[{\"id\": \"schema\", \"template\": {" +
												"\"apiVersion\": rgd.spec.schema.group + \"/\" + rgd.spec.schema.apiVersion," +
												"\"kind\": rgd.spec.schema.kind," +
												"\"metadata\": {\"name\": instance.metadata.name, \"namespace\": instance.metadata.namespace}" +
												"}}]" +
												" + rgd.spec.nodes" +
												" + [{\"id\": \"statusContrib\", \"template\": {" +
												"\"apiVersion\": rgd.spec.schema.group + \"/\" + rgd.spec.schema.apiVersion," +
												"\"kind\": rgd.spec.schema.kind," +
												"\"metadata\": {\"name\": instance.metadata.name, \"namespace\": instance.metadata.namespace}," +
												"\"status\": has(rgd.spec.schema.status) ? rgd.spec.schema.status : {}" +
												"}}]" +
											"}"
										}
									}}
								]}`,
							},
						},
					},
				},
			},
		},
	}
}

func buildRGDCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "resourcegraphdefinitions.test.kro.run"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.kro.run",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "resourcegraphdefinitions",
				Singular: "resourcegraphdefinition",
				Kind:     "ResourceGraphDefinition",
				ListKind: "ResourceGraphDefinitionList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: ptr(true),
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}
}

func waitForResource(ctx context.Context, c client.Client, key types.NamespacedName, obj *unstructured.Unstructured) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		return true, nil
	})
}

// waitForSettle polls until a resource's resourceVersion is stable across
// two consecutive checks. This replaces time.Sleep for "wait for reconcile
// to finish" patterns — it observes completion rather than guessing duration.
func waitForSettle(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName) error {
	var lastRV string
	stableCount := 0
	return wait.PollUntilContextTimeout(ctx, 150*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		rv := obj.GetResourceVersion()
		if rv == lastRV {
			stableCount++
			return stableCount >= 3, nil // stable across 3 checks (~450ms)
		}
		lastRV = rv
		stableCount = 0
		return false, nil
	})
}

// waitForAbsence polls to confirm a resource does NOT exist. It checks several
// times over the duration to be confident the resource won't appear.
// Uses a context-based ticker instead of time.Sleep to honor cancellation.
func waitForAbsence(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName, duration time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil // success: resource stayed absent for the full duration
		case <-ticker.C:
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(gvk)
			if err := c.Get(ctx, key, obj); err == nil {
				return fmt.Errorf("resource %s/%s exists but should be absent", key.Namespace, key.Name)
			}
		}
	}
}

// assertManagedBy checks that a resource has identity labels indicating it's
// managed by the named Graph. Uses the DNS subdomain identity label scheme.
func assertManagedBy(t *testing.T, obj *unstructured.Unstructured, graphName string) {
	t.Helper()
	labels := obj.GetLabels()
	// Check that at least one identity label exists for this graph.
	found := false
	for key, val := range labels {
		if strings.HasSuffix(key, "."+graphName+"."+obj.GetNamespace()+".internal.kro.run/role") {
			found = true
			assert.Contains(t, []string{"owns", "contributes"}, val,
				"%s should have valid role label for Graph %s", obj.GetName(), graphName)
			break
		}
	}
	assert.True(t, found,
		"%s should be managed by Graph %s (no identity label found)", obj.GetName(), graphName)
}

func createNamespace(t *testing.T) string {
	t.Helper()
	ns := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"generateName": "graph-test-",
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, ns))
	t.Cleanup(func() {
		_ = k8sClient.Delete(context.Background(), ns)
	})
	t.Logf("created namespace: %s", ns.GetName())
	return ns.GetName()
}

func ptr[T any](v T) *T { return &v }

func buildSimpleAppCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "simpleapps.test.kro.run"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "test.kro.run",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "simpleapps",
				Singular: "simpleapp",
				Kind:     "SimpleApp",
				ListKind: "SimpleAppList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: ptr(true),
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}
}

// graphReady returns true if the Graph's Ready condition is True.
func graphReady(g *unstructured.Unstructured) bool {
	status, _ := g.Object["status"].(map[string]any)
	if status == nil {
		return false
	}
	conditions, _ := status["conditions"].([]any)
	cond, found := findCondition(conditions, "Ready")
	if !found {
		return false
	}
	return cond["status"] == "True"
}

// graphReadyReason returns the Ready condition's reason string, or "" if not found.
func graphReadyReason(g *unstructured.Unstructured) string {
	status, _ := g.Object["status"].(map[string]any)
	if status == nil {
		return ""
	}
	conditions, _ := status["conditions"].([]any)
	cond, found := findCondition(conditions, "Ready")
	if !found {
		return ""
	}
	reason, _ := cond["reason"].(string)
	return reason
}

// graphReadyStatus returns the Ready condition's status string, or "" if not found.
func graphReadyStatus(g *unstructured.Unstructured) string {
	status, _ := g.Object["status"].(map[string]any)
	if status == nil {
		return ""
	}
	conditions, _ := status["conditions"].([]any)
	cond, found := findCondition(conditions, "Ready")
	if !found {
		return ""
	}
	s, _ := cond["status"].(string)
	return s
}

// waitForGraphReady polls until the Graph's Ready condition is True.
func waitForGraphReady(ctx context.Context, c client.Client, key types.NamespacedName) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := c.Get(ctx, key, g); err != nil {
			return false, nil
		}
		return graphReady(g), nil
	})
}

// waitForGraphReadyReason polls until the Graph's Ready condition has the expected reason.
func waitForGraphReadyReason(ctx context.Context, c client.Client, key types.NamespacedName, reason string) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := c.Get(ctx, key, g); err != nil {
			return false, nil
		}
		return graphReadyReason(g) == reason, nil
	})
}

// waitForGraphReadyStatus polls until the Graph's Ready condition has the expected status (True/False/Unknown).
func waitForGraphReadyStatus(ctx context.Context, c client.Client, key types.NamespacedName, status string) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := c.Get(ctx, key, g); err != nil {
			return false, nil
		}
		return graphReadyStatus(g) == status, nil
	})
}

// findCondition finds a condition by type from a conditions slice.
// Returns the condition map and true if found, nil and false otherwise.
func findCondition(conditions []any, condType string) (map[string]any, bool) {
	for _, c := range conditions {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cMap["type"] == condType {
			return cMap, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// GraphRevision CRD + helpers
// ---------------------------------------------------------------------------

// waitForRevision polls until a GraphRevision with the given name exists.
func waitForRevision(ctx context.Context, c client.Client, key types.NamespacedName) (*unstructured.Unstructured, error) {
	rev := &unstructured.Unstructured{}
	rev.SetGroupVersionKind(GraphRevisionGVK)
	err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, rev); err != nil {
			return false, nil
		}
		return true, nil
	})
	return rev, err
}

// waitForRevisionCondition polls until a GraphRevision has a specific condition status.
func waitForRevisionCondition(ctx context.Context, c client.Client, key types.NamespacedName, condType, expectedStatus string) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		rev := &unstructured.Unstructured{}
		rev.SetGroupVersionKind(GraphRevisionGVK)
		if err := c.Get(ctx, key, rev); err != nil {
			return false, nil
		}
		status, _ := rev.Object["status"].(map[string]any)
		if status == nil {
			return false, nil
		}
		conditions, _ := status["conditions"].([]any)
		cond, found := findCondition(conditions, condType)
		if !found {
			return false, nil
		}
		return cond["status"] == expectedStatus, nil
	})
}

// assertRevisionLabels checks that a GraphRevision has the expected ownership labels.
func assertRevisionLabels(t *testing.T, rev *unstructured.Unstructured, graphName string, generation int64) {
	t.Helper()
	labels := rev.GetLabels()
	assert.Equal(t, graphName, labels[graphcontroller.LabelRevisionGraphName],
		"revision should have graph-name label")
	assert.Equal(t, fmt.Sprintf("%d", generation), labels[graphcontroller.LabelGraphGeneration],
		"revision should have graph-generation label")
	assert.NotEmpty(t, labels[graphcontroller.LabelRevisionHash],
		"revision should have content hash label")
}

// countRevisions returns the number of GraphRevisions for a Graph in a namespace.
func countRevisions(ctx context.Context, c client.Client, graphName, namespace string) (int, error) {
	revisions, err := graphcontroller.ListRevisionsForTest(ctx, c, graphName, namespace)
	if err != nil {
		return 0, err
	}
	return len(revisions), nil
}
