// metric.go implements the metric: node type — a prometheus metric driven by
// CEL evaluation. The metric value is an explicit CEL expression that must
// evaluate to a number. Labels are direct CEL expressions evaluated in
// normal scope — no implicit iteration. Propagation-driven: re-evaluates
// when upstream dependencies change.
package graphcontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ellistarn/kro/experimental/controller/graph"
)

// ---------------------------------------------------------------------------
// MetricStore — per-controller lifecycle for prometheus metrics
// ---------------------------------------------------------------------------

// MetricStore manages prometheus GaugeVec registrations across Graph instances.
// Metrics persist across reconcile cycles (stateless evaluator, stateful metric).
// Thread-safe — multiple reconcile workers may update concurrently.
//
// Each metric gets its own prometheus.Registry. This isolates label mutations:
// when a metric's label keys change, its registry is replaced without affecting
// other metrics or the controller's infrastructure metrics. The MetricStore
// implements prometheus.Collector — the metrics server collects from it to
// expose all graph metrics on /metrics.
type MetricStore struct {
	mu sync.Mutex

	// metrics maps (graphKey, metricName) → registered metric state.
	// graphKey is "namespace/name" of the Graph object.
	metrics map[metricKey]*metricState
}

type metricKey struct {
	GraphKey   string // "namespace/name"
	MetricName string // prometheus metric name
}

type metricState struct {
	registry   *prometheus.Registry
	gauge      *prometheus.GaugeVec
	labelNames []string           // sorted label names (registration order)
	lastLabels prometheus.Labels   // previous label combination (for stale cleanup on non-forEach)
}

// NewMetricStore creates a MetricStore. Register it as a prometheus.Collector
// with the metrics server to expose graph metrics on /metrics.
func NewMetricStore() *MetricStore {
	return &MetricStore{
		metrics: make(map[metricKey]*metricState),
	}
}

// Gather implements prometheus.Gatherer. Iterates all per-metric registries
// and merges their metric families. Used by unit tests to verify metric state.
func (s *MetricStore) Gather() ([]*dto.MetricFamily, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var all []*dto.MetricFamily
	for _, state := range s.metrics {
		mfs, err := state.registry.Gather()
		if err != nil {
			continue // skip broken metrics, don't fail the whole scrape
		}
		all = append(all, mfs...)
	}
	return all, nil
}

// Describe implements prometheus.Collector. Sends nothing — the MetricStore
// is an "unchecked" collector because the set of metrics is dynamic (metrics
// are created/destroyed at runtime as Graphs are reconciled).
func (s *MetricStore) Describe(chan<- *prometheus.Desc) {}

// Collect implements prometheus.Collector. Snapshots gauge pointers under the
// lock, then collects outside the lock to minimize critical section duration.
func (s *MetricStore) Collect(ch chan<- prometheus.Metric) {
	s.mu.Lock()
	gauges := make([]*prometheus.GaugeVec, 0, len(s.metrics))
	for _, state := range s.metrics {
		gauges = append(gauges, state.gauge)
	}
	s.mu.Unlock()

	for _, g := range gauges {
		g.Collect(ch)
	}
}

// Cleanup removes all metrics registered for a given Graph key.
// Called when a Graph is deleted.
func (s *MetricStore) Cleanup(graphKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.metrics {
		if key.GraphKey == graphKey {
			delete(s.metrics, key)
		}
	}
}

// Remove unregisters a single metric for a given Graph key.
// Called during prune when a metric node is removed from the spec or
// its metric name changes between revisions.
func (s *MetricStore) Remove(graphKey, metricName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.metrics, metricKey{GraphKey: graphKey, MetricName: metricName})
}

// Reset clears all label series from a metric registered for a given Graph
// key and metric name. Called before forEach metric iteration to ensure stale
// dimensions from previous cycles are removed — only actively-emitted series
// survive each reconcile cycle.
func (s *MetricStore) Reset(graphKey, metricName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := metricKey{GraphKey: graphKey, MetricName: metricName}
	if state, ok := s.metrics[key]; ok {
		state.gauge.Reset()
		state.lastLabels = nil
	}
}

// getOrCreate returns the GaugeVec for the given key, creating and registering
// it if necessary. If the label set has changed (metric definition mutated),
// the old per-metric registry is discarded and a fresh one is created.
//
// Returns an error if the metric name is already claimed by a different Graph.
func (s *MetricStore) getOrCreate(key metricKey, labelNames []string, help string) (*metricState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.metrics[key]; ok {
		if labelsMatch(existing.labelNames, labelNames) {
			return existing, nil
		}
		// Label set changed — discard old registry, create fresh one below.
		delete(s.metrics, key)
	}

	// Check for metric name collision with a different graph.
	for k := range s.metrics {
		if k.MetricName == key.MetricName && k.GraphKey != key.GraphKey {
			return nil, fmt.Errorf("metric name %q is already used by graph %q", key.MetricName, k.GraphKey)
		}
	}

	if help == "" {
		help = fmt.Sprintf("Graph metric: %s (graph: %s)", key.MetricName, key.GraphKey)
	}

	reg := prometheus.NewRegistry()
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: key.MetricName,
			Help: help,
		},
		labelNames,
	)
	if err := reg.Register(gauge); err != nil {
		return nil, fmt.Errorf("registering metric %q: %w", key.MetricName, err)
	}

	state := &metricState{
		registry:   reg,
		gauge:      gauge,
		labelNames: labelNames,
	}
	s.metrics[key] = state
	return state, nil
}

func labelsMatch(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// reconcileMetric — node handler
// ---------------------------------------------------------------------------

// reconcileMetric evaluates a metric node: evaluates the value expression to
// get a number, evaluates label expressions, and sets the gauge.
// For non-forEach metrics with labels, stale label combinations from
// previous cycles are cleaned up.
func reconcileMetric(ctx context.Context, node graph.Node, eval *evaluator, store *MetricStore, graphKey string) error {
	logger := log.FromContext(ctx)
	metric := node.Metric
	if metric == nil {
		return fmt.Errorf("metric node %s: missing MetricBody", node.ID)
	}

	// Evaluate the value expression — must return a number.
	val, err := eval.evalString(metric.Value)
	if err != nil {
		return fmt.Errorf("metric %s: evaluating value: %w", node.ID, err)
	}
	numVal, err := toFloat64(val)
	if err != nil {
		return fmt.Errorf("metric %s: value must be a number, got %T (%v)", node.ID, val, val)
	}

	// Sorted label names for consistent registration and emission order.
	labelNames := sortedKeys(metric.Labels)

	// Get or create the prometheus GaugeVec.
	key := metricKey{GraphKey: graphKey, MetricName: metric.Name}
	state, err := store.getOrCreate(key, labelNames, metric.Help)
	if err != nil {
		return fmt.Errorf("metric %s: %w", node.ID, err)
	}

	if len(labelNames) == 0 {
		// No labels — single metric value.
		state.gauge.WithLabelValues().Set(numVal)
		logger.V(1).Info("metric evaluated", "node", node.ID, "name", metric.Name, "value", numVal)
		return nil
	}

	// Evaluate label expressions.
	labelValues := make(prometheus.Labels, len(labelNames))
	for _, labelName := range labelNames {
		expr := metric.Labels[labelName]
		lval, err := eval.evalString(expr)
		if err != nil {
			return fmt.Errorf("metric %s: evaluating label %q: %w", node.ID, labelName, err)
		}
		labelValues[labelName] = convertLabelValue(lval)
	}

	// Clean up stale dimensions: for non-forEach metric nodes, if the label
	// values changed from the previous cycle, delete the old series.
	if state.lastLabels != nil {
		oldKey := serializeLabels(state.lastLabels, labelNames)
		newKey := serializeLabels(labelValues, labelNames)
		if oldKey != newKey {
			state.gauge.Delete(state.lastLabels)
		}
	}

	// Set gauge value with labels.
	state.gauge.With(labelValues).Set(numVal)
	state.lastLabels = labelValues

	logger.V(1).Info("metric evaluated", "node", node.ID, "name", metric.Name, "value", numVal, "labels", labelValues)
	return nil
}

// reconcileForEachMetricChild evaluates a metric for a single forEach child.
// Always uses Add() — after Reset() at the start of the forEach loop, each
// child accumulates its value into the gauge. This supports both:
//   - "count by group": forEach over items, value=1, labels from item → count per bucket
//   - "sum by group": forEach over items, value=${item.amount}, labels from item → sum per bucket
//   - "value per group": forEach over distinct groups, value=computed → works because Add(x) from 0 == x
func reconcileForEachMetricChild(ctx context.Context, node graph.Node, eval *evaluator, store *MetricStore, graphKey string) error {
	logger := log.FromContext(ctx)
	metric := node.Metric
	if metric == nil {
		return fmt.Errorf("metric node %s: missing MetricBody", node.ID)
	}

	// Evaluate the value expression — must return a number.
	val, err := eval.evalString(metric.Value)
	if err != nil {
		return fmt.Errorf("metric %s: evaluating value: %w", node.ID, err)
	}
	numVal, err := toFloat64(val)
	if err != nil {
		return fmt.Errorf("metric %s: value must be a number, got %T (%v)", node.ID, val, val)
	}

	labelNames := sortedKeys(metric.Labels)
	key := metricKey{GraphKey: graphKey, MetricName: metric.Name}
	state, err := store.getOrCreate(key, labelNames, metric.Help)
	if err != nil {
		return fmt.Errorf("metric %s: %w", node.ID, err)
	}

	if len(labelNames) == 0 {
		// No labels — accumulate: add this child's value to the metric.
		state.gauge.WithLabelValues().Add(numVal)
		logger.V(1).Info("forEach metric child evaluated", "node", node.ID, "name", metric.Name, "childValue", numVal)
		return nil
	}

	// Evaluate label expressions.
	labelValues := make(prometheus.Labels, len(labelNames))
	for _, labelName := range labelNames {
		expr := metric.Labels[labelName]
		lval, err := eval.evalString(expr)
		if err != nil {
			return fmt.Errorf("metric %s: evaluating label %q: %w", node.ID, labelName, err)
		}
		labelValues[labelName] = convertLabelValue(lval)
	}

	// With labels: Add — multiple forEach children may share label combinations.
	// After Reset(), Add(x) from 0 accumulates correctly for both:
	// - "count by group" (multiple children adding 1 to the same bucket)
	// - "value per distinct group" (one child adding N to a unique bucket)
	state.gauge.With(labelValues).Add(numVal)

	logger.V(1).Info("forEach metric child evaluated", "node", node.ID, "name", metric.Name, "value", numVal, "labels", labelValues)
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// toFloat64 converts a CEL evaluation result to float64.
func toFloat64(val any) (float64, error) {
	switch v := val.(type) {
	case float64:
		return v, nil
	case int64:
		return float64(v), nil
	case int:
		return float64(v), nil
	case uint64:
		return float64(v), nil
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", val)
	}
}

// convertLabelValue converts a CEL evaluation result to a string suitable
// for a prometheus label value.
func convertLabelValue(val any) string {
	switch v := val.(type) {
	case string:
		return v
	case bool:
		return fmt.Sprintf("%t", v)
	case int64:
		return fmt.Sprintf("%d", v)
	case float64:
		return fmt.Sprintf("%g", v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

// serializeLabels creates a consistent string key from label values for comparison.
func serializeLabels(labels prometheus.Labels, sortedNames []string) string {
	parts := make([]string, len(sortedNames))
	for i, name := range sortedNames {
		parts[i] = name + "=" + labels[name]
	}
	return strings.Join(parts, ",")
}

// sortedKeys returns the sorted keys of a map.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
