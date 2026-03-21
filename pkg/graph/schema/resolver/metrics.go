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

package resolver

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// TTL-cached (fallback) resolver metrics.
	cacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_hits_total",
		Help: "Total number of schema resolver cache hits",
	})
	cacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_misses_total",
		Help: "Total number of schema resolver cache misses",
	})

	cacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "schema_resolver_cache_size",
		Help: "Current number of entries in the schema resolver cache",
	})

	cacheEvictionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_evictions_total",
		Help: "Total number of entries evicted from the schema resolver cache",
	})

	apiCallDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "schema_resolver_api_call_duration_seconds",
		Help:    "Duration of API calls to fetch schemas",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms to ~10s
	})

	singleflightDeduplicatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_singleflight_deduplicated_total",
		Help: "Total number of requests that were deduplicated by singleflight",
	})

	schemaResolutionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_errors_total",
		Help: "Total number of schema resolution errors",
	})

	// CRD informer-backed resolver metrics.
	crdCacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_crd_cache_hits_total",
		Help: "Total number of CRD schema resolver cache hits",
	})
	crdCacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_crd_cache_misses_total",
		Help: "Total number of CRD schema resolver cache misses",
	})
	crdCacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "schema_resolver_crd_cache_size",
		Help: "Current number of entries in the CRD schema resolver cache",
	})
	crdCacheEvictionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_crd_cache_evictions_total",
		Help: "Total number of entries evicted from the CRD schema resolver cache",
	})
	crdExtractionDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "schema_resolver_crd_extraction_duration_seconds",
		Help:    "Duration of CRD schema extraction from informer cache",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 12), // 0.1ms to ~0.4s
	})
	crdExtractionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_crd_extraction_errors_total",
		Help: "Total number of CRD schema extraction errors",
	})
	crdSingleflightDeduplicatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_crd_singleflight_deduplicated_total",
		Help: "Total number of CRD resolver requests deduplicated by singleflight",
	})
)

// MustRegister registers the metrics with the given Prometheus registry
func MustRegister(registry prometheus.Registerer) {
	registry.MustRegister(
		// TTL-cached (fallback) resolver.
		cacheHitsTotal,
		cacheMissesTotal,
		cacheSize,
		cacheEvictionsTotal,
		apiCallDuration,
		singleflightDeduplicatedTotal,
		schemaResolutionErrorsTotal,
		// CRD informer-backed resolver.
		crdCacheHitsTotal,
		crdCacheMissesTotal,
		crdCacheSize,
		crdCacheEvictionsTotal,
		crdExtractionDuration,
		crdExtractionErrorsTotal,
		crdSingleflightDeduplicatedTotal,
	)
}

// For now, register with the default registry
//
// TODO(a-hilaly): rework all kro custom metrics to use a custom registry, and
// register them all somewhere central.
func init() {
	MustRegister(metrics.Registry)
}
