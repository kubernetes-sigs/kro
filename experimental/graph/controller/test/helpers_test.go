package graphcontroller_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	"sigs.k8s.io/controller-runtime/pkg/client"

	graphcontroller "github.com/kubernetes-sigs/kro/docs/design/graph/controller"
)

// GraphGVK is a local alias for the exported controller GVK.
var GraphGVK = graphcontroller.GraphGVK

// --- Helpers ---

func buildGraphCRD() *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "graphs.kro.run"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "kro.run",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   "graphs",
				Singular: "graph",
				Kind:     "Graph",
				ListKind: "GraphList",
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
			"apiVersion": "kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "rgd-controller",
				"namespace": namespace,
			},
			"spec": map[string]any{
				"resources": []any{
					map[string]any{
						"id": "rgds",
						"externalRef": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "ResourceGraphDefinition",
							"metadata": map[string]any{
								"selector": map[string]any{},
							},
						},
					},
					map[string]any{
						"id": "controllers",
						"forEach": map[string]any{
							"rgd": "${rgds}",
						},
						"template": map[string]any{
							"apiVersion": "kro.run/v1alpha1",
							"kind":       "Graph",
							"metadata": map[string]any{
								"name": "${rgd.metadata.name}-controller",
							},
							"spec": map[string]any{
								"resources": `${[
									{"id": "rgd", "externalRef": {
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
													"openAPIV3Schema": "${simpleSchema.toOpenAPI(rgd.spec.schema, rgd.spec.resources)}"
												}
											}]
										}
									}},
									{"id": "instances", "externalRef": {
										"apiVersion": "${rgd.spec.schema.group}/${rgd.spec.schema.apiVersion}",
										"kind": "${rgd.spec.schema.kind}",
										"metadata": {"selector": {}}
									}},
									{"id": "instanceGraphs", "forEach": {"instance": "${instances}"},
									 "template": {
										"apiVersion": "kro.run/v1alpha1",
										"kind": "Graph",
										"metadata": {"name": "${instance.metadata.name}-${rgd.spec.schema.kind.lowerAscii()}"},
										"spec": {
											"resources": "${" +
												"[{\"id\": \"schema\", \"externalRef\": {" +
												"\"apiVersion\": rgd.spec.schema.group + \"/\" + rgd.spec.schema.apiVersion," +
												"\"kind\": rgd.spec.schema.kind," +
												"\"metadata\": {\"name\": instance.metadata.name, \"namespace\": instance.metadata.namespace}" +
												"}}]" +
												" + rgd.spec.resources" +
												" + [{\"id\": \"statusContrib\", \"contribution\": true, \"template\": {" +
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

var ensureRGDCRDOnce sync.Once

// ensureRGDCRD installs the RGD CRD idempotently. Multiple parallel tests
// that need the RGD CRD can all call this; only the first creates it.
func ensureRGDCRD(t *testing.T) {
	t.Helper()
	ensureRGDCRDOnce.Do(func() {
		rgdCRD := buildRGDCRD()
		err := k8sClient.Create(ctx, rgdCRD)
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("creating RGD CRD: %v", err)
		}
	})
	require.NoError(t, waitForCRD(ctx, k8sClient, "resourcegraphdefinitions.test.kro.run"))
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
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		return true, nil
	})
}

// waitForAbsence polls to confirm a resource does NOT exist. It checks several
// times over the duration to be confident the resource won't appear.
func waitForAbsence(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, obj); err == nil {
			return fmt.Errorf("resource %s/%s exists but should be absent", key.Namespace, key.Name)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// assertManagedBy checks that a resource has labels indicating it's managed by the named Graph.
func assertManagedBy(t *testing.T, obj *unstructured.Unstructured, graphName string) {
	t.Helper()
	labels := obj.GetLabels()
	assert.Equal(t, graphName, labels["kro.run/graph-name"],
		"%s should be managed by Graph %s", obj.GetName(), graphName)
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
