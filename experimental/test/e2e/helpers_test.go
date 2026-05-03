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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	graphcontroller "github.com/ellistarn/kro/experimental/controller"
	graphpkg "github.com/ellistarn/kro/experimental/controller/graph"
)

// GraphGVK is a local alias for the exported controller GVK.
var GraphGVK = graphcontroller.GraphGVK

// GraphRevisionGVK is a local alias for the exported revision GVK.
var GraphRevisionGVK = graphcontroller.GraphRevisionGVK

// --- Helpers ---

func waitForCRD(ctx context.Context, c client.Client, name string, timeout ...time.Duration) error {
	t := 30 * time.Second
	if len(timeout) > 0 {
		t = timeout[0]
	}
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, t, true, func(ctx context.Context) (bool, error) {
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
						"watch": map[string]any{
							"apiVersion": "test.kro.run/v1alpha1",
							"kind":       "ResourceGraphDefinition",
							// Watch namespace follows k8s list/watch semantics:
							// absent = all namespaces. Tests pin to their own
							// namespace to keep parallel runs isolated.
							"metadata": map[string]any{"namespace": namespace},
							"selector": map[string]any{},
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
									{"id": "rgd", "ref": {
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
									{"id": "instances", "watch": {
										"apiVersion": "${rgd.spec.schema.group}/${rgd.spec.schema.apiVersion}",
										"kind": "${rgd.spec.schema.kind}",
										"metadata": {"namespace": rgd.metadata.namespace},
										"selector": {}
									}},
									{"id": "instanceGraphs", "forEach": {"instance": "${instances}"},
									 "template": {
										"apiVersion": "experimental.kro.run/v1alpha1",
										"kind": "Graph",
										"metadata": {"name": "${instance.metadata.name}-${rgd.spec.schema.kind.lowerAscii()}"},
										"spec": {
											"nodes": "${" +
												"[{\"id\": \"schema\", \"ref\": {" +
												"\"apiVersion\": rgd.spec.schema.group + \"/\" + rgd.spec.schema.apiVersion," +
												"\"kind\": rgd.spec.schema.kind," +
												"\"metadata\": {\"name\": instance.metadata.name, \"namespace\": instance.metadata.namespace}" +
												"}}]" +
												" + rgd.spec.nodes" +
												" + [{\"id\": \"statusContrib\", \"patch\": {" +
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

// updateWithRetry fetches the latest version of an unstructured resource,
// applies the mutate function, and retries on conflict. This eliminates
// flakes caused by the controller updating the object between Get and Update.
func updateWithRetry(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName, mutate func(*unstructured.Unstructured)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
		mutate(obj)
		return c.Update(ctx, obj)
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

// waitForDeletion polls until a resource returns NotFound. Use this when a
// resource has been deleted and you need to wait for teardown to complete.
// Unlike waitForAbsence (which proves something never appears), this observes
// the completion of a deletion that is expected to succeed.
func waitForDeletion(ctx context.Context, c client.Client, gvk schema.GroupVersionKind, key types.NamespacedName) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		err := c.Get(ctx, key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, nil
	})
}

// referenceLabelValue returns the identity label value stamped on obj for
// the named Graph — "template" or "patch" — or ("", false) if no identity
// label is present for that Graph.
//
// The identity label key encodes the stamping Graph's name and namespace,
// so a resource touched by multiple Graphs has one label per Graph; this
// helper finds the one for graphName.
func referenceLabelValue(obj *unstructured.Unstructured, graphName string) (string, bool) {
	suffix := "." + graphName + "." + obj.GetNamespace() + ".internal.kro.run/type"
	for key, val := range obj.GetLabels() {
		if strings.HasSuffix(key, suffix) {
			return val, true
		}
	}
	return "", false
}

// assertManagedBy checks that a resource has identity labels indicating it's
// managed by the named Graph. Uses the DNS subdomain identity label scheme.
func assertManagedBy(t *testing.T, obj *unstructured.Unstructured, graphName string) {
	t.Helper()
	val, ok := referenceLabelValue(obj, graphName)
	if !assert.True(t, ok,
		"%s should be managed by Graph %s (no identity label found)", obj.GetName(), graphName) {
		return
	}
	assert.Contains(t, []string{"template", "patch"}, val,
		"%s should have valid role label for Graph %s", obj.GetName(), graphName)
}

// assertReferenceClassification asserts that a resource's identity label for
// the named Graph matches want ("template" or "patch"). Use this to pin the
// classification a Graph has arrived at — e.g., to verify a Patch→Template
// transition has completed.
func assertReferenceClassification(t *testing.T, obj *unstructured.Unstructured, graphName, want string) {
	t.Helper()
	val, ok := referenceLabelValue(obj, graphName)
	if !assert.True(t, ok,
		"%s should carry an identity label for Graph %s", obj.GetName(), graphName) {
		return
	}
	assert.Equal(t, want, val,
		"%s identity label for Graph %s should be %q, got %q", obj.GetName(), graphName, want, val)
}

// uniqueGroup returns a unique API group for test CRD isolation. CRDs are
// cluster-scoped, so parallel tests that create CRDs with the same group
// would collide. Each call generates a different group (e.g.,
// "apps-xk4wz.test.kro.run"), ensuring the derived CRD name
// (<plural>.<group>) is unique per test.
func uniqueGroup() string {
	return fmt.Sprintf("apps-%s.test.kro.run", rand.String(5))
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

// buildCustomCRD returns a namespace-scoped CRD with the given identity.
// Used when tests need a CRD with a unique group for isolation.
func buildCustomCRD(name, group, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:   plural,
				Singular: strings.ToLower(kind),
				Kind:     kind,
				ListKind: kind + "List",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name:    "v1alpha1",
				Served:  true,
				Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{
					OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
						Type:                   "object",
						XPreserveUnknownFields: ptr.To(true),
					},
				},
				Subresources: &apiextensionsv1.CustomResourceSubresources{
					Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
				},
			}},
		},
	}
}
// conditionField extracts a field from a condition by type. Returns "" if
// the condition or field is not found.
func conditionField(g *unstructured.Unstructured, condType, field string) string {
	status, _ := g.Object["status"].(map[string]any)
	if status == nil {
		return ""
	}
	conditions, _ := status["conditions"].([]any)
	cond, found := findCondition(conditions, condType)
	if !found {
		return ""
	}
	v, _ := cond[field].(string)
	return v
}

// graphReady returns true if the Graph's Ready condition is True.
func graphReady(g *unstructured.Unstructured) bool { return conditionField(g, "Ready", "status") == "True" }

// graphReadyMessage returns the Ready condition's message string, or "" if not found.
func graphReadyMessage(g *unstructured.Unstructured) string { return conditionField(g, "Ready", "message") }

// graphReadyReason returns the Ready condition's reason string, or "" if not found.
func graphReadyReason(g *unstructured.Unstructured) string { return conditionField(g, "Ready", "reason") }

// graphReadyStatus returns the Ready condition's status string, or "" if not found.
func graphReadyStatus(g *unstructured.Unstructured) string { return conditionField(g, "Ready", "status") }

// graphCompiledReason returns the reason from the Compiled condition.
func graphCompiledReason(g *unstructured.Unstructured) string { return conditionField(g, "Compiled", "reason") }

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

// waitForGraphCompiledStatus polls until the Graph's Compiled condition
// reaches the given status ("True" or "False").
func waitForGraphCompiledStatus(ctx context.Context, c client.Client, key types.NamespacedName, wantStatus string) error {
	return wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		g := &unstructured.Unstructured{}
		g.SetGroupVersionKind(GraphGVK)
		if err := c.Get(ctx, key, g); err != nil {
			return false, nil
		}
		status, _ := g.Object["status"].(map[string]any)
		conditions, _ := status["conditions"].([]any)
		cond, ok := findCondition(conditions, "Compiled")
		if !ok {
			return false, nil
		}
		s, _ := cond["status"].(string)
		return s == wantStatus, nil
	})
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

// assertRevisionLabels checks that a GraphRevision has the expected ownership labels.
func assertRevisionLabels(t *testing.T, rev *unstructured.Unstructured, graphName string, generation int64) {
	t.Helper()
	labels := rev.GetLabels()
	assert.Equal(t, graphName, labels[graphpkg.LabelRevisionGraphName],
		"revision should have graph-name label")
	assert.Equal(t, fmt.Sprintf("%d", generation), labels[graphpkg.LabelGraphGeneration],
		"revision should have graph-generation label")
}

// countRevisions returns the number of GraphRevisions for a Graph in a namespace.
func countRevisions(ctx context.Context, c client.Client, graphName, namespace string) (int, error) {
	revisions, err := graphcontroller.ListRevisionsForTest(ctx, c, graphName, namespace)
	if err != nil {
		return 0, err
	}
	return len(revisions), nil
}


