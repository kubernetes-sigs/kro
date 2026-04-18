//go:build load

// Load tests for the Graph controller. These exercise the controller under
// sustained load with many Graphs, measuring CPU and memory behavior.
//
// Run: go test ./experimental/controller/test -tags load -run TestLoad -v -timeout 10m
//
// These are excluded from presubmit (build tag "load") because they're slow
// (~2-3 minutes) and produce noisy results (GC timing, envtest overhead).
// Run explicitly when investigating performance or validating changes.
package graphcontroller_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
)

// ---------------------------------------------------------------------------
// Configuration — thresholds and scale parameters
//
// These are policy. Adjust when the controller's memory model changes.
// ---------------------------------------------------------------------------

const (
	// Scale parameters
	loadGraphCount       = 50 // number of Graphs to create
	loadNodesPerGraph    = 5  // nodes per Graph (1 source + N dependents)
	loadBatchSize        = 5  // create this many Graphs per batch
	loadSettleTime       = 15 * time.Second
	loadDeleteSettleTime = 15 * time.Second
	loadConvergeTimeout  = 120 * time.Second // per-Graph convergence timeout — generous under load
)

// ---------------------------------------------------------------------------
// Test: Load profile — CPU and memory under sustained Graph creation
// ---------------------------------------------------------------------------

// TestLoadProfile creates many Graphs, measures memory at each scale point,
// scrapes heap profiles from the controller process, and checks for leaks
// after deletion. This exercises the full reconcile path under load:
// compilation, DAG construction, template evaluation, SSA apply, watch setup,
// and steady-state skip paths.
func TestLoadProfile(t *testing.T) {
	ns := createNamespace(t)

	// Discover the pprof address from the binary. The startBinary in
	// main_test.go would need to pass it, but since load tests share the
	// same TestMain, we need the pprof address. We'll discover it by
	// convention: the load test expects PPROF_ADDR env var to be set.
	// If not set, we skip heap profiling but still do RSS tracking.
	pprofAddr := os.Getenv("PPROF_ADDR")
	if pprofAddr == "" {
		t.Log("PPROF_ADDR not set — heap profiling disabled. Set it to the controller's pprof address for full profiling.")
		t.Log("Falling back to RSS tracking only.")
	}

	// Find the controller PID for RSS tracking.
	controllerPID := findControllerPID(t)

	// Record baseline memory.
	baselineRSS := getProcessRSS(t, controllerPID)
	t.Logf("Baseline: controller PID=%d, RSS=%s", controllerPID, formatBytes(baselineRSS))

	if pprofAddr != "" {
		saveHeapProfile(t, pprofAddr, "baseline")
	}

	// -----------------------------------------------------------------------
	// Phase 1: Create Graphs in batches, measure memory growth
	// -----------------------------------------------------------------------
	type scalePoint struct {
		graphCount int
		rss        int64
		timestamp  time.Time
	}
	var scalePoints []scalePoint

	graphNames := make([]string, 0, loadGraphCount)
	cmGVK := schema.GroupVersionKind{Group: "", Version: "v1", Kind: "ConfigMap"}

	for batch := 0; batch < loadGraphCount/loadBatchSize; batch++ {
		batchStart := batch * loadBatchSize
		for i := 0; i < loadBatchSize; i++ {
			idx := batchStart + i
			name := fmt.Sprintf("load-%03d", idx)
			graphNames = append(graphNames, name)

			graph := buildLoadGraph(name, ns, loadNodesPerGraph)
			require.NoError(t, k8sClient.Create(ctx, graph),
				"creating Graph %s", name)
		}

		// Wait for this batch to converge — use a longer timeout under load.
		for i := 0; i < loadBatchSize; i++ {
			idx := batchStart + i
			name := fmt.Sprintf("load-%03d", idx)
			err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, loadConvergeTimeout, true, func(ctx context.Context) (bool, error) {
				g := &unstructured.Unstructured{}
				g.SetGroupVersionKind(GraphGVK)
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, g); err != nil {
					return false, nil
				}
				return graphReady(g), nil
			})
			require.NoError(t, err, "waiting for Graph %s", name)
		}

		// Force a GC in the test process (not the controller, but keeps our
		// measurements clean from test-side allocations).
		runtime.GC()

		rss := getProcessRSS(t, controllerPID)
		point := scalePoint{
			graphCount: batchStart + loadBatchSize,
			rss:        rss,
			timestamp:  time.Now(),
		}
		scalePoints = append(scalePoints, point)
		t.Logf("Scale point: %d Graphs, RSS=%s (+%s from baseline)",
			point.graphCount, formatBytes(rss), formatBytes(rss-baselineRSS))
	}

	// Let everything settle.
	time.Sleep(loadSettleTime)

	// -----------------------------------------------------------------------
	// Phase 2: Steady-state measurement
	// -----------------------------------------------------------------------
	steadyRSS := getProcessRSS(t, controllerPID)
	t.Logf("Steady state: %d Graphs, RSS=%s", loadGraphCount, formatBytes(steadyRSS))

	if pprofAddr != "" {
		saveHeapProfile(t, pprofAddr, "peak")
	}

	// Verify all managed resources exist.
	managedCount := 0
	for _, name := range graphNames {
		for j := 0; j < loadNodesPerGraph; j++ {
			cm := &unstructured.Unstructured{}
			cm.SetGroupVersionKind(cmGVK)
			nodeName := fmt.Sprintf("%s-node-%d", name, j)
			err := k8sClient.Get(ctx,
				types.NamespacedName{Name: nodeName, Namespace: ns}, cm)
			if err == nil {
				managedCount++
			}
		}
	}
	expectedManaged := loadGraphCount * loadNodesPerGraph
	t.Logf("Managed resources: %d/%d", managedCount, expectedManaged)
	assert.Equal(t, expectedManaged, managedCount,
		"all managed resources should exist")

	// -----------------------------------------------------------------------
	// Phase 3: Delete all Graphs, measure memory release
	// -----------------------------------------------------------------------
	t.Log("Deleting all Graphs...")
	for _, name := range graphNames {
		graph := &unstructured.Unstructured{}
		graph.SetGroupVersionKind(GraphGVK)
		graph.SetName(name)
		graph.SetNamespace(ns)
		require.NoError(t, k8sClient.Delete(ctx, graph),
			"deleting Graph %s", name)
	}

	// Wait for deletion to propagate — poll cache stats until drained.
	if pprofAddr != "" {
		require.NoError(t, waitForCacheDrain(t, pprofAddr, 60*time.Second),
			"cache should drain after deleting all Graphs")
	} else {
		time.Sleep(loadDeleteSettleTime)
	}

	// Force GC in the controller via pprof if available.
	if pprofAddr != "" {
		triggerRemoteGC(t, pprofAddr)
		time.Sleep(2 * time.Second)
		// Second GC pass — finalizers may have freed objects on the first pass.
		triggerRemoteGC(t, pprofAddr)
		freeOSMemory(t, pprofAddr)
		time.Sleep(1 * time.Second)
	}

	postDeleteRSS := getProcessRSS(t, controllerPID)
	t.Logf("Post-deletion: RSS=%s", formatBytes(postDeleteRSS))

	if pprofAddr != "" {
		saveHeapProfile(t, pprofAddr, "post-delete")
		logCacheStats(t, pprofAddr, "post-cycle-1")
	}

	// -----------------------------------------------------------------------
	// Phase 4: Report
	// -----------------------------------------------------------------------
	t.Log("")
	t.Log("========== LOAD TEST REPORT ==========")
	t.Logf("Configuration: %d Graphs × %d nodes = %d managed resources",
		loadGraphCount, loadNodesPerGraph, expectedManaged)
	t.Logf("Baseline RSS:     %s", formatBytes(baselineRSS))
	t.Logf("Peak RSS:         %s", formatBytes(steadyRSS))
	t.Logf("Post-delete RSS:  %s", formatBytes(postDeleteRSS))
	t.Logf("Growth:           %s (%.1f MB)",
		formatBytes(steadyRSS-baselineRSS),
		float64(steadyRSS-baselineRSS)/(1024*1024))

	perGraphBytes := int64(0)
	if loadGraphCount > 0 {
		perGraphBytes = (steadyRSS - baselineRSS) / int64(loadGraphCount)
	}
	t.Logf("Per-Graph cost:   %s", formatBytes(perGraphBytes))

	leaked := postDeleteRSS - baselineRSS
	t.Logf("Residual after deletion: %s (%.1f%% of peak growth)",
		formatBytes(leaked),
		float64(leaked)/float64(steadyRSS-baselineRSS)*100)

	t.Log("")
	t.Log("Scale curve:")
	for _, p := range scalePoints {
		bar := strings.Repeat("█", int(float64(p.rss-baselineRSS)/float64(steadyRSS-baselineRSS)*40))
		t.Logf("  %3d Graphs: %s %s", p.graphCount, formatBytes(p.rss), bar)
	}

	if pprofAddr != "" {
		t.Log("")
		t.Logf("Heap profiles saved to current directory:")
		t.Logf("  go tool pprof -top heap-baseline.prof")
		t.Logf("  go tool pprof -top heap-peak.prof")
		t.Logf("  go tool pprof -top heap-post-delete.prof")
		t.Logf("  go tool pprof -diff_base=heap-baseline.prof heap-peak.prof")
	}
	t.Log("=======================================")

	// -----------------------------------------------------------------------
	// Phase 5: Second create-delete cycle to detect proportional leaks.
	// If Extend() during finalization registers entries in a global scope,
	// the heap grows with each cycle. If the 2nd cycle matches the 1st,
	// the finalization allocations are bounded (one-time cost per expression).
	// -----------------------------------------------------------------------
	t.Log("")
	t.Log("Starting cycle 2 — detecting proportional leaks...")

	cycle2Names := make([]string, 0, loadGraphCount)
	for batch := 0; batch < loadGraphCount/loadBatchSize; batch++ {
		batchStart := batch * loadBatchSize
		for i := 0; i < loadBatchSize; i++ {
			idx := batchStart + i
			name := fmt.Sprintf("c2-%03d", idx)
			cycle2Names = append(cycle2Names, name)
			graph := buildLoadGraph(name, ns, loadNodesPerGraph)
			require.NoError(t, k8sClient.Create(ctx, graph), "creating Graph %s", name)
		}
		for i := 0; i < loadBatchSize; i++ {
			idx := batchStart + i
			name := fmt.Sprintf("c2-%03d", idx)
			err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, loadConvergeTimeout, true, func(ctx context.Context) (bool, error) {
				g := &unstructured.Unstructured{}
				g.SetGroupVersionKind(GraphGVK)
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, g); err != nil {
					return false, nil
				}
				return graphReady(g), nil
			})
			require.NoError(t, err, "cycle 2: waiting for Graph %s", name)
		}
	}
	t.Logf("Cycle 2: all %d Graphs converged", loadGraphCount)

	// Delete all cycle 2 Graphs.
	for _, name := range cycle2Names {
		graph := &unstructured.Unstructured{}
		graph.SetGroupVersionKind(GraphGVK)
		graph.SetName(name)
		graph.SetNamespace(ns)
		require.NoError(t, k8sClient.Delete(ctx, graph), "deleting Graph %s", name)
	}
	if pprofAddr != "" {
		require.NoError(t, waitForCacheDrain(t, pprofAddr, 60*time.Second),
			"cache should drain after cycle 2 deletion")
	} else {
		time.Sleep(loadDeleteSettleTime)
	}

	if pprofAddr != "" {
		triggerRemoteGC(t, pprofAddr)
		time.Sleep(2 * time.Second)
		triggerRemoteGC(t, pprofAddr)
		freeOSMemory(t, pprofAddr)
		time.Sleep(1 * time.Second)
		saveHeapProfile(t, pprofAddr, "post-cycle2")
	}

	cycle2RSS := getProcessRSS(t, controllerPID)
	if pprofAddr != "" {
		logCacheStats(t, pprofAddr, "post-cycle-2")
	}
	t.Logf("Post-cycle-2 RSS: %s (post-cycle-1 was %s)", formatBytes(cycle2RSS), formatBytes(postDeleteRSS))
	t.Logf("Cycle-over-cycle growth: %s", formatBytes(cycle2RSS-postDeleteRSS))

	if pprofAddr != "" {
		t.Log("Compare: go tool pprof -diff_base=heap-post-delete.prof heap-post-cycle2.prof")
	}
}

// ---------------------------------------------------------------------------
// Graph builder
// ---------------------------------------------------------------------------

// buildLoadGraph creates a Graph with a chain of ConfigMap nodes.
// Node 0 is a standalone ConfigMap. Nodes 1..N each reference the previous
// node's data, forming a dependency chain.
func buildLoadGraph(name, namespace string, nodeCount int) *unstructured.Unstructured {
	nodes := make([]any, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodeID := fmt.Sprintf("node%d", i)
		nodeName := fmt.Sprintf("%s-node-%d", name, i)
		data := map[string]any{}
		if i == 0 {
			data["value"] = "root"
		} else {
			prevID := fmt.Sprintf("node%d", i-1)
			// Node 0 has data.value; nodes 1+ have data.value too (derived from prev).
			// Use a consistent field name so downstream CEL expressions resolve.
			data["value"] = fmt.Sprintf("${%s.data.value}", prevID)
		}
		nodes[i] = map[string]any{
			"id": nodeID,
			"template": map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": nodeName},
				"data":       data,
			},
		}
	}

	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "experimental.kro.run/v1alpha1",
			"kind":       "Graph",
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"nodes": nodes,
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Process monitoring helpers
// ---------------------------------------------------------------------------

// findControllerPID reads the controller PID from CONTROLLER_PID env var,
// set by startBinary in main_test.go.
func findControllerPID(t *testing.T) int {
	t.Helper()
	pidStr := os.Getenv("CONTROLLER_PID")
	require.NotEmpty(t, pidStr, "CONTROLLER_PID not set — is TestMain running?")
	pid, err := strconv.Atoi(pidStr)
	require.NoError(t, err, "parsing CONTROLLER_PID: %s", pidStr)
	return pid
}

// getProcessRSS returns the RSS (resident set size) in bytes for a process.
func getProcessRSS(t *testing.T, pid int) int64 {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Logf("warning: could not read RSS for PID %d: %v", pid, err)
		return 0
	}
	kbStr := strings.TrimSpace(string(out))
	kb, err := strconv.ParseInt(kbStr, 10, 64)
	if err != nil {
		t.Logf("warning: could not parse RSS: %q", kbStr)
		return 0
	}
	return kb * 1024 // ps reports in KB
}

// formatBytes formats bytes into a human-readable string.
func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024*1024:
		return fmt.Sprintf("%.1f GB", float64(b)/(1024*1024*1024))
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// ---------------------------------------------------------------------------
// pprof helpers
// ---------------------------------------------------------------------------

// saveHeapProfile scrapes a heap profile from the controller's pprof endpoint
// and saves it to a file.
func saveHeapProfile(t *testing.T, pprofAddr, label string) {
	t.Helper()
	url := fmt.Sprintf("http://%s/debug/pprof/heap", pprofAddr)
	resp, err := http.Get(url)
	if err != nil {
		t.Logf("warning: could not scrape heap profile: %v", err)
		return
	}
	defer resp.Body.Close()

	filename := fmt.Sprintf("heap-%s.prof", label)
	f, err := os.Create(filename)
	if err != nil {
		t.Logf("warning: could not create profile file: %v", err)
		return
	}
	defer f.Close()
	io.Copy(f, resp.Body)
	t.Logf("Saved heap profile: %s", filename)
}

// triggerRemoteGC triggers a garbage collection in the controller process
// by fetching the heap profile (which calls runtime.GC() as a side effect
// of the debug/pprof/heap handler).
func triggerRemoteGC(t *testing.T, pprofAddr string) {
	t.Helper()
	url := fmt.Sprintf("http://%s/debug/pprof/heap?gc=1", pprofAddr)
	resp, err := http.Get(url)
	if err != nil {
		t.Logf("warning: could not trigger remote GC: %v", err)
		return
	}
	resp.Body.Close()
}

// freeOSMemory forces the controller process to release unused memory to the OS.
// Without this, Go holds freed pages (MADV_FREE on darwin), inflating RSS.
func freeOSMemory(t *testing.T, pprofAddr string) {
	t.Helper()
	url := fmt.Sprintf("http://%s/debug/freeosmemory", pprofAddr)
	resp, err := http.Get(url)
	if err != nil {
		t.Logf("warning: could not free OS memory: %v", err)
		return
	}
	resp.Body.Close()
}

// logCacheStats scrapes the cache stats endpoint and logs the result.
func logCacheStats(t *testing.T, pprofAddr, label string) {
	t.Helper()
	url := fmt.Sprintf("http://%s/debug/cachestats", pprofAddr)
	resp, err := http.Get(url)
	if err != nil {
		t.Logf("warning: could not get cache stats: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	t.Logf("Cache stats [%s]: %s", label, strings.TrimSpace(string(body)))
}

// waitForCacheDrain polls the cache stats endpoint until both compiled and
// instance counts are zero, or the timeout expires.
func waitForCacheDrain(t *testing.T, pprofAddr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s/debug/cachestats", pprofAddr)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err != nil {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		s := strings.TrimSpace(string(body))
		if s == "compiled=0 instances=0" {
			t.Logf("Cache drained: %s", s)
			return nil
		}
		t.Logf("Waiting for cache drain: %s", s)
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("cache did not drain within %s", timeout)
}
