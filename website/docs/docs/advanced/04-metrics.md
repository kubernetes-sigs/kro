---
sidebar_position: 4
---

# Controller Metrics

kro exposes Prometheus metrics for monitoring controller health and performance. These metrics are available at the metrics endpoint (default `:8078/metrics`), configurable via Helm values.

## Enabling Metrics

```yaml
metrics:
  service:
    create: true
    port: 8080
  serviceMonitor:
    enabled: true  # If using Prometheus Operator
    interval: 1m
```

## Dynamic Controller Metrics

| Metric | Type | Description | Stability |
|--------|------|-------------|-----------|
| `dynamic_controller_reconcile_total` | Counter | Total number of reconciliations per GVR | ALPHA |
| `dynamic_controller_reconcile_duration_seconds` | Histogram | Duration of reconciliations per GVR in seconds | ALPHA |
| `dynamic_controller_requeue_total` | Counter | Total number of requeues per GVR and requeue type | ALPHA |
| `dynamic_controller_handler_errors_total` | Counter | Total number of handler errors per GVR | ALPHA |
| `dynamic_controller_queue_length` | Gauge | Current length of the workqueue | ALPHA |
| `dynamic_controller_gvr_count` | Gauge | Number of instance GVRs currently managed by the controller | ALPHA |
| `dynamic_controller_handler_count_total` | Gauge | Number of active handlers by type (parent or child) | ALPHA |
| `dynamic_controller_handler_attach_total` | Counter | Total number of handler attachments by type | ALPHA |
| `dynamic_controller_handler_detach_total` | Counter | Total number of handler detachments by type | ALPHA |
| `dynamic_controller_informer_events_total` | Counter | Total number of events processed by informers per GVR and event type | ALPHA |
| `dynamic_controller_informer_sync_duration_seconds` | Histogram | Duration of informer cache sync per GVR in seconds | ALPHA |

## Schema Resolver Metrics

| Metric | Type | Description | Stability |
|--------|------|-------------|-----------|
| `schema_resolver_cache_hits_total` | Counter | Total number of schema resolver cache hits | ALPHA |
| `schema_resolver_cache_misses_total` | Counter | Total number of schema resolver cache misses | ALPHA |
| `schema_resolver_cache_size` | Gauge | Current number of entries in the schema resolver cache | ALPHA |
| `schema_resolver_cache_evictions_total` | Counter | Total number of entries evicted from the schema resolver cache | ALPHA |
| `schema_resolver_api_call_duration_seconds` | Histogram | Duration of API calls to fetch schemas in seconds | ALPHA |
| `schema_resolver_singleflight_deduplicated_total` | Counter | Total number of requests deduplicated by singleflight | ALPHA |
| `schema_resolver_errors_total` | Counter | Total number of schema resolution errors | ALPHA |

## ResourceGraphDefinition Controller Metrics

| Metric | Type | Description | Stability |
|--------|------|-------------|-----------|
| `rgd_graph_build_total` | Counter | Total number of RGD graph validations during reconciliation | ALPHA |
| `rgd_graph_build_duration_seconds` | Histogram | Duration of RGD graph validations in seconds | ALPHA |
| `rgd_graph_build_errors_total` | Counter | Total number of RGD graph validation errors | ALPHA |
| `rgd_state_transitions_total` | Counter | Total number of RGD state transitions (labels: from, to) | ALPHA |
| `rgd_deletions_total` | Counter | Total number of RGD deletions | ALPHA |
| `rgd_deletion_duration_seconds` | Histogram | Duration of RGD deletions in seconds | ALPHA |

## Controller Runtime Metrics

The RGD reconciler uses controller-runtime and exposes its [standard metrics](https://book.kubebuilder.io/reference/metrics.html):

| Metric | Type | Description | Stability |
|--------|------|-------------|-----------|
| `controller_runtime_reconcile_total` | Counter | Total number of reconciliations per controller | STABLE |
| `controller_runtime_reconcile_time_seconds` | Histogram | Length of time per reconciliation per controller | STABLE |
| `controller_runtime_reconcile_errors_total` | Counter | Total number of reconciliation errors per controller | STABLE |
| `controller_runtime_terminal_reconcile_errors_total` | Counter | Total number of terminal reconciliation errors per controller | STABLE |
| `controller_runtime_reconcile_panics_total` | Counter | Total number of reconciliation panics per controller | STABLE |
| `controller_runtime_max_concurrent_reconciles` | Gauge | Maximum number of concurrent reconciles per controller | STABLE |
| `controller_runtime_active_workers` | Gauge | Number of currently used workers per controller | STABLE |

## Workqueue Metrics

| Metric | Type | Description | Stability |
|--------|------|-------------|-----------|
| `workqueue_adds_total` | Counter | Total number of adds handled by workqueue | STABLE |
| `workqueue_depth` | Gauge | Current depth of workqueue | STABLE |
| `workqueue_queue_duration_seconds` | Histogram | How long in seconds an item stays in workqueue before being requested | STABLE |
| `workqueue_work_duration_seconds` | Histogram | How long in seconds processing an item from workqueue takes | STABLE |
| `workqueue_retries_total` | Counter | Total number of retries handled by workqueue | STABLE |
| `workqueue_longest_running_processor_seconds` | Gauge | How many seconds has the longest running processor for workqueue been running | STABLE |
| `workqueue_unfinished_work_seconds` | Gauge | How many seconds of work has been done that is in progress and hasn't been observed by work_duration | STABLE |
