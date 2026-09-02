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

package metrics

import (
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// registeredFamilies is the set of metric family names Register installs.
//
// Metric names are part of kro's operator-facing surface: dashboards, alerts
// and recording rules reference them by name. Dropping or renaming one is a
// breaking change for whoever operates the controller, and it is invisible
// both at compile time and at scrape time — a removed series simply stops
// appearing, so panels go blank and alerts silently stop evaluating rather
// than failing loudly.
//
// This list exists so that any such change has to be deliberate. If you add a
// metric, add it here. If you remove or rename one, update this list in the
// same commit and call the change out in the release notes so operators can
// migrate their queries.
var registeredFamilies = []string{
	// CEL
	"cel_expr_eval_duration_seconds",
	"cel_expr_eval_total",

	// Dynamic controller
	"dynamic_controller_gvr_count",
	"dynamic_controller_handler_attach_total",
	"dynamic_controller_handler_count_total",
	"dynamic_controller_handler_detach_total",
	"dynamic_controller_handler_errors_total",
	"dynamic_controller_informer_events_total",
	"dynamic_controller_informer_sync_duration_seconds",
	"dynamic_controller_instance_watch_count",
	"dynamic_controller_queue_length",
	"dynamic_controller_reconcile_duration_seconds",
	"dynamic_controller_reconcile_panics_total",
	"dynamic_controller_reconcile_total",
	"dynamic_controller_requeue_total",
	"dynamic_controller_route_match_total",
	"dynamic_controller_route_total",
	"dynamic_controller_watch_count",
	"dynamic_controller_watch_request_count",

	// Instance controller
	"instance_condition_current_status_seconds",
	"instance_graph_resolution_failures_total",
	"instance_graph_resolution_pending_total",
	"instance_graph_resolution_success_total",
	"instance_reconcile_duration_seconds",
	"instance_reconcile_errors_total",
	"instance_reconcile_total",
	"instance_state_transitions_total",

	// Runtime
	"runtime_collection_size",
	"runtime_creation_duration_seconds",
	"runtime_creation_total",
	"runtime_node_eval_duration_seconds",
	"runtime_node_eval_errors_total",
	"runtime_node_eval_total",
	"runtime_node_ignored_check_total",
	"runtime_node_ignored_total",
	"runtime_node_not_ready_total",
	"runtime_node_ready_check_total",

	// RGD controller
	"rgd_deletion_duration_seconds",
	"rgd_deletions_total",
	"rgd_graph_build_duration_seconds",
	"rgd_graph_build_errors_total",
	"rgd_graph_build_total",
	"rgd_graph_revision_gc_deleted_total",
	"rgd_graph_revision_gc_errors_total",
	"rgd_graph_revision_issue_total",
	"rgd_graph_revision_registry_miss_total",
	"rgd_graph_revision_resolution_total",
	"rgd_graph_revision_wait_total",
	"rgd_state_transitions_total",

	// Graph-engine watch router
	"graphengine_route_match_total",
	"graphengine_route_total",
	"graphengine_watch_owner_count",
	"graphengine_watch_request_count",

	// GraphRevision controller
	"graph_revision_activation_deferred_total",
	"graph_revision_compile_duration_seconds",
	"graph_revision_compile_total",
	"graph_revision_finalizer_evictions_total",
	"graph_revision_status_update_errors_total",

	// Schema resolver
	"schema_resolver_api_call_duration_seconds",
	"schema_resolver_cache_evictions_total",
	"schema_resolver_cache_hits_total",
	"schema_resolver_cache_misses_total",
	"schema_resolver_cache_size",
	"schema_resolver_errors_total",
	"schema_resolver_singleflight_deduplicated_total",

	// Revision registry
	"graph_revision_registry_entries",
	"graph_revision_registry_evictions_total",
	"graph_revision_registry_transitions_total",

	// Client-go
	"rest_client_rate_limiter_duration_seconds",
	"rest_client_request_duration_seconds",
	"rest_client_request_retries_total",
	"rest_client_request_size_bytes",
	"rest_client_response_size_bytes",
}

// TestRegisterInstallsDocumentedFamilies asserts that Register installs every
// metric family in registeredFamilies, and no others. Both directions matter:
// a missing name means a series operators may depend on has disappeared, and
// an unexpected name means a new metric was added without being recorded here.
func TestRegisterInstallsDocumentedFamilies(t *testing.T) {
	rec := &descRecorder{names: map[string]struct{}{}}
	Register(rec)

	want := map[string]struct{}{}
	for _, name := range registeredFamilies {
		want[name] = struct{}{}
	}

	var missing, unexpected []string
	for name := range want {
		if _, ok := rec.names[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range rec.names {
		if _, ok := want[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(unexpected)

	if len(missing) > 0 {
		t.Errorf("metric families are no longer registered: %v\n"+
			"Removing a metric is a breaking change for dashboards and alerts. If this is "+
			"intentional, drop the name from registeredFamilies in this file and note the "+
			"removal in the release notes.", missing)
	}
	if len(unexpected) > 0 {
		t.Errorf("metric families are registered but not documented here: %v\n"+
			"Add them to registeredFamilies so the operator-facing metric surface stays "+
			"reviewable.", unexpected)
	}
}

// descRecorder is a prometheus.Registerer that records the fully-qualified
// names a collector describes instead of collecting samples. Gathering from a
// real registry would only surface families that already have observations,
// which would make label-bearing counters invisible until first use.
type descRecorder struct {
	names map[string]struct{}
}

func (r *descRecorder) Register(c prometheus.Collector) error {
	ch := make(chan *prometheus.Desc, 64)
	go func() {
		c.Describe(ch)
		close(ch)
	}()
	for desc := range ch {
		if name := fqNameOf(desc); name != "" {
			r.names[name] = struct{}{}
		}
	}
	return nil
}

func (r *descRecorder) MustRegister(cs ...prometheus.Collector) {
	for _, c := range cs {
		if err := r.Register(c); err != nil {
			panic(err)
		}
	}
}

func (r *descRecorder) Unregister(prometheus.Collector) bool { return false }

// fqNameOf extracts a Desc's fully-qualified metric name. Desc keeps fqName
// unexported and only exposes it through String(), which renders as
// `Desc{fqName: "name", help: ...}`.
func fqNameOf(d *prometheus.Desc) string {
	const marker = `fqName: "`
	s := d.String()
	start := strings.Index(s, marker)
	if start < 0 {
		return ""
	}
	s = s[start+len(marker):]
	before, _, ok := strings.Cut(s, `"`)
	if !ok {
		return ""
	}
	return before
}
