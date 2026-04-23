package resync_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var GraphGVK = schema.GroupVersionKind{
	Group:   "experimental.kro.run",
	Version: "v1alpha1",
	Kind:    "Graph",
}

func createNamespace(t *testing.T) string {
	t.Helper()
	ns := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]any{
				"generateName": "resync-test-",
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

func waitForResource(ctx context.Context, c client.Client, key types.NamespacedName, obj *unstructured.Unstructured) error {
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, obj); err != nil {
			return false, nil
		}
		return true, nil
	})
}

// waitForSettle polls until a resource's resourceVersion is stable across
// two consecutive checks.
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
			return stableCount >= 3, nil
		}
		lastRV = rv
		stableCount = 0
		return false, nil
	})
}

// updateWithRetry fetches the latest version, applies the mutate function,
// and retries on conflict.
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
