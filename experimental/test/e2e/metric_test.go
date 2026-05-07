package graphcontroller_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ---------------------------------------------------------------------------
// Metric nodes (type: gauge) — prometheus gauge driven by CEL evaluation
// ---------------------------------------------------------------------------

// TestGaugeBasic proves that a metric node with no labels emits a single
// prometheus gauge whose value equals size(source list).
func TestMetricBasic(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create some ConfigMaps that the watch will observe.
	for i := 0; i < 3; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-test-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-basic"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-basic",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-basic",
							},
						},
					},
					map[string]any{
						"id": "cmCount",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_basic_total",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-basic", Namespace: ns}))

	// Verify the gauge is exposed on the metrics endpoint.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_basic_total", "3"))
}

// TestGaugeWithLabels proves that a forEach + metric node slices the source
// list by distinct label values and emits one gauge series per unique label
// combination, each with value = count of items in that group.
func TestMetricWithLabels(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps with different "tier" labels to test grouping.
	tiers := []string{"frontend", "frontend", "backend", "backend", "backend"}
	for i, tier := range tiers {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-labels-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-labels", "tier": tier},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-labels",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-labels",
							},
						},
					},
					map[string]any{
						"id": "cmByTier",
						"forEach": map[string]any{
							"tier": "${cms.map(c, c.metadata.labels.tier).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_by_tier",
							"labels": map[string]any{
								"tier": "${tier}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.tier == tier))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-labels", Namespace: ns}))

	// Verify the gauge is exposed with correct label dimensions.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_by_tier{tier="frontend"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_by_tier{tier="backend"}`, "3"))
}

// TestGaugeWithFilter proves that filtering the source list with CEL works.
func TestMetricWithFilter(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps — some with "active" label, some without.
	for i := 0; i < 5; i++ {
		labels := map[string]any{"test": "gauge-filter"}
		if i < 3 {
			labels["active"] = "true"
		}
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-filter-cm-%d", i),
					"namespace": ns,
					"labels":    labels,
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-filter",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-filter",
							},
						},
					},
					map[string]any{
						"id": "activeCms",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_filter_active",
							"value": "${size(cms.filter(c, has(c.metadata.labels.active)))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-filter", Namespace: ns}))

	// Only the 3 active ConfigMaps should be counted.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_filter_active", "3"))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// waitForMetric polls the controller's /metrics endpoint until the given
// metric line appears with the expected value. The metricMatch is a
// substring that must appear on the same line as the value.
func waitForMetric(t *testing.T, metricMatch string, expectedValue string) error {
	t.Helper()
	if metricsAddr == "" {
		t.Skip("metrics endpoint not available")
	}

	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		resp, err := http.Get("http://" + metricsAddr + "/metrics")
		if err != nil {
			return false, nil // retry
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, nil
		}

		lines := strings.Split(string(body), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, metricMatch) {
				// Line format: metric_name{labels} value
				// or: metric_name value
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					actual := parts[len(parts)-1]
					if actual == expectedValue {
						return true, nil
					}
					t.Logf("metric %q found but value=%s, want %s", metricMatch, actual, expectedValue)
				}
				return false, nil
			}
		}
		return false, nil // metric not found yet
	})
}

// TestGaugeReactive proves that gauge values update when watched resources change.
// Exercises add → add → delete lifecycle.
func TestMetricReactive(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-reactive",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-reactive",
							},
						},
					},
					map[string]any{
						"id": "cmCount",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_reactive_total",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-reactive", Namespace: ns}))

	// Initially 0 items.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_reactive_total", "0"))

	// Add first ConfigMap → gauge should go to 1.
	cm1 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-reactive-cm-1",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-reactive"},
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cm1))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_reactive_total", "1"))

	// Add second ConfigMap → gauge should go to 2.
	cm2 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-reactive-cm-2",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-reactive"},
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cm2))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_reactive_total", "2"))

	// Delete first ConfigMap → gauge should drop back to 1.
	require.NoError(t, k8sClient.Delete(ctx, cm1))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_reactive_total", "1"))
}

// TestGaugeStaleDimensionCleanup proves that when a label combination disappears
// (e.g., all items with tier=frontend are deleted), the corresponding gauge
// series is removed from the metrics endpoint.
func TestMetricStaleDimensionCleanup(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps with two tiers.
	for i, tier := range []string{"alpha", "beta"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-stale-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-stale", "tier": tier},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-stale",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-stale",
							},
						},
					},
					map[string]any{
						"id": "byTier",
						"forEach": map[string]any{
							"tier": "${cms.map(c, c.metadata.labels.tier).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_stale_by_tier",
							"labels": map[string]any{
								"tier": "${tier}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.tier == tier))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-stale", Namespace: ns}))

	// Both dimensions should exist.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_stale_by_tier{tier="alpha"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_stale_by_tier{tier="beta"}`, "1"))

	// Delete the "beta" ConfigMap.
	betaCM := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-stale-cm-1",
				"namespace": ns,
			},
		},
	}
	require.NoError(t, k8sClient.Delete(ctx, betaCM))

	// The "beta" series should disappear, "alpha" should remain.
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_stale_by_tier{tier="beta"}`))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_stale_by_tier{tier="alpha"}`, "1"))
}

// waitForMetricAbsence polls until the given metric line is no longer present
// on the /metrics endpoint.
func waitForMetricAbsence(t *testing.T, metricMatch string) error {
	t.Helper()
	if metricsAddr == "" {
		t.Skip("metrics endpoint not available")
	}

	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 30*time.Second, true, func(ctx2 context.Context) (bool, error) {
		resp, err := http.Get("http://" + metricsAddr + "/metrics")
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, nil
		}

		lines := strings.Split(string(body), "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, metricMatch) {
				return false, nil // still present, keep polling
			}
		}
		return true, nil // absent
	})
}

// TestGaugeForEach proves that metric nodes work inside forEach — each
// forEach iteration evaluates the metric with its own scope, and all
// children contribute to the same GaugeVec with distinct label values.
func TestMetricForEach(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps in two "environments".
	for i, env := range []string{"staging", "staging", "prod", "prod", "prod"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-foreach-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-foreach", "env": env},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-foreach",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-foreach",
							},
						},
					},
					// forEach over environments, count CMs per env
					map[string]any{
						"id": "countPerEnv",
						"forEach": map[string]any{
							"env": "${['staging', 'prod']}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_foreach_count",
							"labels": map[string]any{
								"environment": "${env}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.env == env))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-foreach", Namespace: ns}))

	// Verify: staging=2, prod=3
	require.NoError(t, waitForMetric(t, `kro_test_gauge_foreach_count{environment="staging"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_foreach_count{environment="prod"}`, "3"))
}

// TestGaugeIncludeWhen proves that a metric node with includeWhen=false is
// excluded and emits no metrics. When the condition flips to true, the
// metric activates.
func TestMetricIncludeWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a "gate" ConfigMap that controls inclusion.
	gate := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-gate",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-includewhen"},
			},
			"data": map[string]any{"enabled": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gate))

	// Create some items to count.
	for i := 0; i < 2; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-iw-item-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-includewhen-item"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-includewhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "gateRef",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "gauge-gate",
								"namespace": ns,
							},
						},
					},
					map[string]any{
						"id": "items",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-includewhen-item",
							},
						},
					},
					map[string]any{
						"id": "conditionalGauge",
						"includeWhen": []any{
							"${gateRef.data.enabled == 'true'}",
						},
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_includewhen",
							"value": "${size(items)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-includewhen", Namespace: ns}))

	// Gauge should NOT be emitted (node is excluded).
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_includewhen"))

	// Flip the gate to true.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "gauge-gate", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "true", "data", "enabled")
		}))

	// Now the gauge should appear with value = 2.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_includewhen", "2"))
}

// TestGaugePropagateWhen proves that a metric node with propagateWhen
// unsatisfied retains its previous state (no evaluation) until the
// condition is met.
func TestMetricPropagateWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a "gate" ConfigMap.
	gate := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-pw-gate",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-propagatewhen"},
			},
			"data": map[string]any{"ready": "false"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gate))

	// Create items to count.
	for i := 0; i < 3; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-pw-item-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-propagatewhen-item"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-propagatewhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "gateRef",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "gauge-pw-gate",
								"namespace": ns,
							},
						},
					},
					map[string]any{
						"id": "items",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-propagatewhen-item",
							},
						},
					},
					map[string]any{
						"id": "gatedGauge",
						"propagateWhen": []any{
							"${gateRef.data.ready == 'true'}",
						},
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_propagatewhen",
							"value": "${size(items)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Graph won't be Ready because the metric is Pending (propagateWhen unsatisfied).
	// Wait for compiled, then verify the metric is absent.
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-propagatewhen", Namespace: ns}, "True"))
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_propagatewhen"))

	// Flip the gate — gauge should now evaluate.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "gauge-pw-gate", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "true", "data", "ready")
		}))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-propagatewhen", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_propagatewhen", "3"))
}

// TestGaugeDeletion proves that when a Graph is deleted, its gauges are
// removed from the /metrics endpoint (no stale metrics left behind).
func TestMetricDeletion(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create items to count.
	for i := 0; i < 2; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-del-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-deletion"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-deletion",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-deletion",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_deletion_total",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-deletion", Namespace: ns}))

	// Gauge should be present.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_deletion_total", "2"))

	// Delete the Graph.
	require.NoError(t, k8sClient.Delete(ctx, graph))

	// Wait for the graph to be gone.
	require.NoError(t, waitForDeletion(ctx, k8sClient,
		schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"},
		types.NamespacedName{Name: "test-gauge-deletion", Namespace: ns}))

	// The metric should disappear from /metrics.
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_deletion_total"))
}

// TestGaugeMultiple proves that multiple metric nodes in the same Graph
// coexist without interfering — each emits its own metric independently.
func TestMetricMultiple(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps with different labels.
	for i, color := range []string{"red", "red", "blue"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-multi-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-multiple", "color": color},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-multiple",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-multiple",
							},
						},
					},
					// First metric: total count, no labels.
					map[string]any{
						"id": "totalCount",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_multi_total",
							"value": "${size(cms)}",
						},
					},
					// Second metric: count by color label.
					map[string]any{
						"id": "byColor",
						"forEach": map[string]any{
							"color": "${cms.map(c, c.metadata.labels.color).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_multi_by_color",
							"labels": map[string]any{
								"color": "${color}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.color == color))}",
						},
					},
					// Third metric: count filtered to red only.
					map[string]any{
						"id": "redOnly",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_multi_red",
							"value": "${size(cms.filter(c, c.metadata.labels.color == 'red'))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-multiple", Namespace: ns}))

	// All three gauges should emit independently.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_multi_total", "3"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multi_by_color{color="red"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multi_by_color{color="blue"}`, "1"))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_multi_red", "2"))
}

// TestGaugeWithDef proves that a metric node can reference a def node's
// computed values — the metric value operates on scope data from upstream
// computations, not just raw watch outputs.
func TestMetricWithDef(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps.
	for i := 0; i < 4; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-def-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-def", "active": fmt.Sprintf("%t", i < 3)},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-def",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-def",
							},
						},
					},
					// Def node computes a filtered list inside a map.
					map[string]any{
						"id": "computed",
						"def": map[string]any{
							"active": "${cms.filter(c, c.metadata.labels.active == 'true')}",
						},
					},
					// Metric references the def node's computed list.
					map[string]any{
						"id": "activeCount",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_def_active",
							"value": "${size(computed.active)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-def", Namespace: ns}))

	// Only 3 of the 4 CMs have active=true.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_def_active", "3"))
}

// TestGaugeMultipleLabels proves that a metric can slice data across multiple
// dimensions simultaneously, producing the cross-product of label values.
// Verifies add/delete transitions update the correct dimension intersections.
func TestMetricMultipleLabels(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps spanning a 2D matrix: env × tier
	// staging/frontend: 1
	// staging/backend:  2
	// prod/frontend:    1
	// prod/backend:     1
	items := []struct {
		env, tier string
	}{
		{"staging", "frontend"},
		{"staging", "backend"},
		{"staging", "backend"},
		{"prod", "frontend"},
		{"prod", "backend"},
	}
	for i, item := range items {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-ml-cm-%d", i),
					"namespace": ns,
					"labels": map[string]any{
						"test": "gauge-multilabel",
						"env":  item.env,
						"tier": item.tier,
					},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-multilabel",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-multilabel",
							},
						},
					},
					map[string]any{
						"id": "byEnvTier",
						"forEach": map[string]any{
							"cm": "${cms}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_multilabel",
							"labels": map[string]any{
								"env":  "${cm.metadata.labels.env}",
								"tier": "${cm.metadata.labels.tier}",
							},
							"value": "${1}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-multilabel", Namespace: ns}))

	// Verify all 4 dimension intersections.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="staging",tier="frontend"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="staging",tier="backend"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="prod",tier="frontend"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="prod",tier="backend"}`, "1"))

	// Add another prod/frontend item — that cell should go from 1 → 2.
	cm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-ml-cm-new",
				"namespace": ns,
				"labels": map[string]any{
					"test": "gauge-multilabel",
					"env":  "prod",
					"tier": "frontend",
				},
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="prod",tier="frontend"}`, "2"))

	// Delete one staging/backend — that cell goes from 2 → 1.
	toDelete := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-ml-cm-1",
				"namespace": ns,
			},
		},
	}
	require.NoError(t, k8sClient.Delete(ctx, toDelete))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_multilabel{env="staging",tier="backend"}`, "1"))
}

// TestGaugeNodeRemoval proves that when a Graph spec is updated to remove a
// metric node, the metric is pruned from the /metrics endpoint. The prune
// phase detects that the superseded revision had a metric node absent from
// the current spec and calls MetricStore.Remove.
func TestMetricNodeRemoval(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create items to count.
	for i := 0; i < 2; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-removal-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-removal"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-removal",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-removal",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_removal_total",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-removal", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_removal_total", "2"))

	// Update the Graph spec to remove the metric node — keep only the watch.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"},
		types.NamespacedName{Name: "test-gauge-removal", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "cms",
					"watch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"namespace": ns,
						},
						"selector": map[string]any{
							"test": "gauge-removal",
						},
					},
				},
			}, "spec", "nodes")
		}))

	// The gauge metric should disappear from /metrics.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-removal", Namespace: ns}))
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_removal_total"))
}

// TestGaugeMetricRename proves that when a metric node's metric name changes,
// the old metric is pruned and the new one appears.
func TestMetricMetricRename(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	for i := 0; i < 3; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-rename-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-rename"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-rename",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-rename",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_old_name",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-rename", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_old_name", "3"))

	// Rename the metric.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"},
		types.NamespacedName{Name: "test-gauge-rename", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "cms",
					"watch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"namespace": ns,
						},
						"selector": map[string]any{
							"test": "gauge-rename",
						},
					},
				},
				map[string]any{
					"id": "count",
					"metric": map[string]any{
						"type":  "gauge",
						"name":  "kro_test_gauge_new_name",
						"value": "${size(cms)}",
					},
				},
			}, "spec", "nodes")
		}))

	// Old metric should disappear, new one should appear.
	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-rename", Namespace: ns}))
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_old_name"))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_new_name", "3"))
}

// TestGaugeForEachStaleDimension proves that when a forEach iteration disappears
// (collection shrinks), the gauge dimensions contributed by that iteration are
// cleaned up. The gauge Reset before forEach iteration ensures only active
// dimensions survive.
func TestMetricForEachStaleDimension(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps in "dev" and "staging" environments.
	for i, env := range []string{"dev", "dev", "staging"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-fe-stale-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-fe-stale", "env": env},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-fe-stale",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-fe-stale",
							},
						},
					},
					map[string]any{
						"id": "countPerEnv",
						"forEach": map[string]any{
							"env": "${cms.map(c, c.metadata.labels.env).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_fe_stale",
							"labels": map[string]any{
								"environment": "${env}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.env == env))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-fe-stale", Namespace: ns}))

	// Both dimensions should exist.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_fe_stale{environment="dev"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_fe_stale{environment="staging"}`, "1"))

	// Delete the staging ConfigMap — the forEach iterates distinct env values
	// from cms. With no staging CMs left, "staging" won't appear in distinct()
	// and its label dimension disappears.
	stagingCM := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-fe-stale-cm-2",
				"namespace": ns,
			},
		},
	}
	require.NoError(t, k8sClient.Delete(ctx, stagingCM))

	// The "staging" dimension should disappear (no items → not in distinct()).
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_fe_stale{environment="staging"}`))
	// "dev" should remain unchanged.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_fe_stale{environment="dev"}`, "2"))
}

// ---------------------------------------------------------------------------
// Additional coverage tests
// ---------------------------------------------------------------------------

// TestGaugeIncludeWhenRevoke proves that when includeWhen flips from true back
// to false, the gauge metric disappears. This exercises the prune path for
// excluded nodes (contagious exclusion removes the metric).
func TestMetricIncludeWhenRevoke(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Gate starts enabled.
	gate := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-revoke-gate",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-revoke"},
			},
			"data": map[string]any{"enabled": "true"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gate))

	for i := 0; i < 2; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-revoke-item-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-revoke-item"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-revoke",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "gateRef",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "gauge-revoke-gate",
								"namespace": ns,
							},
						},
					},
					map[string]any{
						"id": "items",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-revoke-item",
							},
						},
					},
					map[string]any{
						"id": "conditionalGauge",
						"includeWhen": []any{
							"${gateRef.data.enabled == 'true'}",
						},
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_revoke",
							"value": "${size(items)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-revoke", Namespace: ns}))

	// Gauge should be present with value 2.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_revoke", "2"))

	// Flip the gate to false — gauge should disappear.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "gauge-revoke-gate", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "false", "data", "enabled")
		}))

	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_revoke"))
}

// TestGaugeZeroValue proves that when a metric's source list becomes empty,
// the metric emits value 0 (not absent). A gauge with no labels set to 0
// is distinguishable from "metric not registered" — important for alerting.
func TestMetricZeroValue(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create one ConfigMap that we'll later delete.
	cm := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-zero-cm-0",
				"namespace": ns,
				"labels":    map[string]any{"test": "gauge-zero"},
			},
			"data": map[string]any{"key": "value"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cm))

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-zero",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-zero",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_zero_total",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-zero", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_zero_total", "1"))

	// Delete the only ConfigMap — gauge should go to 0, NOT disappear.
	require.NoError(t, k8sClient.Delete(ctx, cm))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_zero_total", "0"))
}

// TestGaugeLabelPartialFailure proves that when a label expression fails for
// some items (field doesn't exist), those items degrade to empty-string label
// values. The metric still works and groups items correctly.
func TestMetricLabelOptionalField(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps: some with "tier" label, some without.
	for i, tier := range []string{"frontend", "", "backend", ""} {
		labels := map[string]any{"test": "gauge-partial"}
		if tier != "" {
			labels["tier"] = tier
		}
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-partial-cm-%d", i),
					"namespace": ns,
					"labels":    labels,
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-partial",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-partial",
							},
						},
					},
					map[string]any{
						"id": "byTier",
						"forEach": map[string]any{
							"tier": "${cms.map(c, has(c.metadata.labels.tier) ? c.metadata.labels.tier : '').distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_partial",
							"labels": map[string]any{
								"tier": "${tier}",
							},
							"value": "${size(cms.filter(c, (has(c.metadata.labels.tier) ? c.metadata.labels.tier : '') == tier))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-partial", Namespace: ns}))

	// Items without "tier" label degrade to tier="" (empty string).
	require.NoError(t, waitForMetric(t, `kro_test_gauge_partial{tier="frontend"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_partial{tier="backend"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_partial{tier=""}`, "2"))
}

// TestGaugeForEachNoLabels proves that a forEach metric with NO labels uses
// the Add() accumulator path and Reset correctly zeroes it each cycle.
// Each forEach child contributes its filtered count additively.
func TestMetricForEachNoLabels(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps in two environments.
	for i, env := range []string{"dev", "dev", "dev", "prod", "prod"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-fenl-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-fenl", "env": env},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-fenl",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-fenl",
							},
						},
					},
					map[string]any{
						"id": "totalPerEnv",
						"forEach": map[string]any{
							"env": "${['dev', 'prod']}",
						},
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_fenl_total",
							"value": "${size(cms.filter(c, c.metadata.labels.env == env))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-fenl", Namespace: ns}))

	// No labels: each child Adds. dev=3 + prod=2 = 5 total.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_fenl_total", "5"))

	// Delete one dev CM — total should drop to 4.
	toDelete := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-fenl-cm-0",
				"namespace": ns,
			},
		},
	}
	require.NoError(t, k8sClient.Delete(ctx, toDelete))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_fenl_total", "4"))
}

// TestGaugeExprNonList proves that when the metric value evaluates to a non-numeric
// value, the node enters an error state and the graph reports the error.
func TestMetricNonNumericValue(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// A def node that produces a string (not a number).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-nonlist",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "data",
						"def": map[string]any{
							"value": "not-a-number",
						},
					},
					map[string]any{
						"id": "badGauge",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_nonlist",
							"value": "${data.value}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Graph should compile but NOT become ready (metric node errors).
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-nonlist", Namespace: ns}, "True"))

	// The metric should NOT be registered (node errors before setting gauge).
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_nonlist"))
}

// TestGaugeSpecAddGauge proves that updating a Graph spec to ADD a metric node
// (where none existed before) causes the metric to appear.
func TestMetricSpecAddGauge(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	for i := 0; i < 3; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-add-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-add"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Create graph with only a watch node — no metric.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-add",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-add",
							},
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-add", Namespace: ns}))

	// Metric should NOT exist yet.
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_add_total"))

	// Update spec to add a metric node.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"},
		types.NamespacedName{Name: "test-gauge-add", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "cms",
					"watch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"namespace": ns,
						},
						"selector": map[string]any{
							"test": "gauge-add",
						},
					},
				},
				map[string]any{
					"id": "count",
					"metric": map[string]any{
						"type":  "gauge",
						"name":  "kro_test_gauge_add_total",
						"value": "${size(cms)}",
					},
				},
			}, "spec", "nodes")
		}))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-add", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_add_total", "3"))
}

// TestGaugeMetricCollision proves that two Graphs declaring the same metric
// name results in a node error on the second graph (prometheus rejects
// duplicate registrations). The first graph continues to work.
func TestMetricMetricCollision(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	for i := 0; i < 2; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-coll-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-collision"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// First graph with the metric name.
	graph1 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-coll-1",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-collision",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_collision_shared",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph1))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph1) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-coll-1", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_collision_shared", "2"))

	// Second graph with the SAME metric name — should fail to register.
	graph2 := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-coll-2",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-collision",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_collision_shared",
							"value": "${size(cms)}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph2))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph2) })

	// Second graph should compile but NOT be ready (gauge registration fails).
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-coll-2", Namespace: ns}, "True"))

	// First graph's metric should still work correctly.
	require.NoError(t, waitForMetric(t, "kro_test_gauge_collision_shared", "2"))
}

// TestGaugeEmptyListWithLabels proves that when the source list is empty but
// forEach labels are defined, no label series are emitted (no phantom zeros per
// label dimension). The metric should have no series at all.
func TestMetricEmptyListWithLabels(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Graph with a watch that will match nothing (empty selector).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-empty-labels",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-empty-labels-nonexistent",
							},
						},
					},
					map[string]any{
						"id": "byTier",
						"forEach": map[string]any{
							"tier": "${cms.map(c, c.metadata.labels.tier).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_empty_labels",
							"labels": map[string]any{
								"tier": "${tier}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.tier == tier))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-empty-labels", Namespace: ns}))

	// With an empty list and forEach labels, no series should be emitted at all.
	// The forEach produces no iterations (empty distinct()), so no data points.
	require.NoError(t, waitForMetricAbsence(t, "kro_test_gauge_empty_labels"))
}

// TestGaugeWithReadyWhen proves that readyWhen works with metric nodes.
// The metric evaluates (sets its value) but the node's readiness is gated
// by readyWhen. Downstream nodes see the metric as not-ready until the
// condition passes.
func TestMetricWithReadyWhen(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create items.
	for i := 0; i < 3; i++ {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-rw-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-readywhen"},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// A metric with readyWhen that references an external condition.
	// The metric emits its value but the node is only "ready" when the
	// gate is satisfied — meaning downstream deps won't see it as ready.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-readywhen",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-readywhen",
							},
						},
					},
					map[string]any{
						"id": "count",
						"metric": map[string]any{
							"type":  "gauge",
							"name":  "kro_test_gauge_readywhen",
							"value": "${size(cms)}",
						},
						// readyWhen: always true (metric just evaluated, nothing to wait for).
						// This proves readyWhen doesn't crash on metric nodes.
						"readyWhen": []any{
							"${true}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-readywhen", Namespace: ns}))
	require.NoError(t, waitForMetric(t, "kro_test_gauge_readywhen", "3"))
}

// TestGaugeForEachChildError proves that when some forEach metric children
// produce empty results (e.g., filter yields nothing for some iteration
// variables), the metric still emits correct values for the children
// that have data. The empty iterations contribute nothing.
func TestMetricForEachChildError(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create ConfigMaps that will be items. Only "good" items exist.
	for i, env := range []string{"good", "good", "good"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-fce-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-fce", "category": env},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// forEach iterates ['good', 'bad']. The 'bad' iteration filters for
	// category=bad which yields an empty list — value is 0.
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-fce",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-fce",
							},
						},
					},
					map[string]any{
						"id": "perCategory",
						"forEach": map[string]any{
							"cat": "${['good', 'bad']}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_fce",
							"labels": map[string]any{
								"category": "${cat}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.category == cat))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-fce", Namespace: ns}))

	// 'good' iteration: 3 items with category=good → emits {category="good"} = 3.
	// 'bad' iteration: 0 items → emits {category="bad"} = 0 (dimension exists, value is zero).
	require.NoError(t, waitForMetric(t, `kro_test_gauge_fce{category="good"}`, "3"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_fce{category="bad"}`, "0"))
}

// TestGaugeForEachPropagateWhenHalt proves that when a forEach metric node has
// propagateWhen that halts mid-iteration, the metric emits only the values from
// children processed before the halt. The Reset at the start of the forEach
// ensures stale values from previous cycles don't persist.
func TestMetricForEachPropagateWhenHalt(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	// Create a gate ConfigMap and items.
	gate := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "gauge-pw-halt-gate",
				"namespace": ns,
			},
			"data": map[string]any{"maxItems": "1"},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, gate))

	for i, env := range []string{"alpha", "beta", "gamma"} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-pw-halt-cm-%d", i),
					"namespace": ns,
					"labels":    map[string]any{"test": "gauge-pw-halt", "env": env},
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// forEach iterates ['alpha', 'beta', 'gamma'] with propagateWhen that
	// halts after processing 1 item (checks size of partial collection).
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-pw-halt",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "gateRef",
						"ref": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":      "gauge-pw-halt-gate",
								"namespace": ns,
							},
						},
					},
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-pw-halt",
							},
						},
					},
					map[string]any{
						"id": "perEnv",
						"forEach": map[string]any{
							"env": "${['alpha', 'beta', 'gamma']}",
						},
						"propagateWhen": []any{
							"${size(perEnv) < int(gateRef.data.maxItems)}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_pw_halt",
							"labels": map[string]any{
								"env": "${env}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.env == env))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	// Graph won't be fully ready (propagateWhen halts, node is Pending).
	require.NoError(t, waitForGraphCompiledStatus(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-pw-halt", Namespace: ns}, "True"))

	// Only the first item ('alpha') should be processed before the halt.
	// After Reset + 1 child: gauge has {env="alpha"} = 1.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_pw_halt{env="alpha"}`, "1"))
	// 'beta' and 'gamma' should NOT be emitted (halted).
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_pw_halt{env="beta"}`))
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_pw_halt{env="gamma"}`))

	// Raise the gate to allow all 3 — gauge should now show all dimensions.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
		types.NamespacedName{Name: "gauge-pw-halt-gate", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(obj.Object, "10", "data", "maxItems")
		}))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-pw-halt", Namespace: ns}))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_pw_halt{env="alpha"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_pw_halt{env="beta"}`, "1"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_pw_halt{env="gamma"}`, "1"))
}

// TestGaugeLabelKeyMutation proves that when a Graph spec update changes the
// label keys on a metric node (e.g., {env: ...} → {tier: ...}), the old label
// dimensions disappear and the new ones appear. This exercises the per-gauge
// registry swap: the old registry is discarded and a fresh one is created
// with the new label schema.
func TestMetricLabelKeyMutation(t *testing.T) {
	t.Parallel()
	ns := createNamespace(t)

	for i, labels := range []map[string]any{
		{"test": "gauge-labelmut", "env": "prod", "tier": "frontend"},
		{"test": "gauge-labelmut", "env": "prod", "tier": "backend"},
		{"test": "gauge-labelmut", "env": "staging", "tier": "frontend"},
	} {
		cm := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      fmt.Sprintf("gauge-labelmut-cm-%d", i),
					"namespace": ns,
					"labels":    labels,
				},
				"data": map[string]any{"key": "value"},
			},
		}
		require.NoError(t, k8sClient.Create(ctx, cm))
	}

	// Start with labels grouping by "env".
	graph := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      "test-gauge-labelmut",
				"namespace": ns,
			},
			"spec": map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "cms",
						"watch": map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"namespace": ns,
							},
							"selector": map[string]any{
								"test": "gauge-labelmut",
							},
						},
					},
					map[string]any{
						"id": "grouped",
						"forEach": map[string]any{
							"env": "${cms.map(c, c.metadata.labels.env).distinct()}",
						},
						"metric": map[string]any{
							"type": "gauge",
							"name": "kro_test_gauge_labelmut",
							"labels": map[string]any{
								"env": "${env}",
							},
							"value": "${size(cms.filter(c, c.metadata.labels.env == env))}",
						},
					},
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, graph))
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, graph) })

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-labelmut", Namespace: ns}))

	// Grouped by env: prod=2, staging=1.
	require.NoError(t, waitForMetric(t, `kro_test_gauge_labelmut{env="prod"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_labelmut{env="staging"}`, "1"))

	// Mutate labels: change from {env} to {tier}.
	require.NoError(t, updateWithRetry(ctx, k8sClient,
		schema.GroupVersionKind{Group: "experimental.kro.run", Version: "v1alpha1", Kind: "Graph"},
		types.NamespacedName{Name: "test-gauge-labelmut", Namespace: ns},
		func(obj *unstructured.Unstructured) {
			_ = unstructured.SetNestedSlice(obj.Object, []any{
				map[string]any{
					"id": "cms",
					"watch": map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata": map[string]any{
							"namespace": ns,
						},
						"selector": map[string]any{
							"test": "gauge-labelmut",
						},
					},
				},
				map[string]any{
					"id": "grouped",
					"forEach": map[string]any{
						"tier": "${cms.map(c, c.metadata.labels.tier).distinct()}",
					},
					"metric": map[string]any{
						"type": "gauge",
						"name": "kro_test_gauge_labelmut",
						"labels": map[string]any{
							"tier": "${tier}",
						},
						"value": "${size(cms.filter(c, c.metadata.labels.tier == tier))}",
					},
				},
			}, "spec", "nodes")
		}))

	require.NoError(t, waitForGraphReady(ctx, k8sClient,
		types.NamespacedName{Name: "test-gauge-labelmut", Namespace: ns}))

	// Old "env" dimensions should be gone; new "tier" dimensions appear.
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_labelmut{env="prod"}`))
	require.NoError(t, waitForMetricAbsence(t, `kro_test_gauge_labelmut{env="staging"}`))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_labelmut{tier="frontend"}`, "2"))
	require.NoError(t, waitForMetric(t, `kro_test_gauge_labelmut{tier="backend"}`, "1"))
}
