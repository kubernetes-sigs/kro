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

import "github.com/prometheus/client_golang/prometheus"

var (
	SchemaResolverCacheHitsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_hits_total",
		Help: "Total number of schema resolver cache hits",
	})
	SchemaResolverCacheMissesTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_misses_total",
		Help: "Total number of schema resolver cache misses",
	})

	SchemaResolverCacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "schema_resolver_cache_size",
		Help: "Current number of entries in the schema resolver cache",
	})

	SchemaResolverCacheEvictionsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_cache_evictions_total",
		Help: "Total number of entries evicted from the schema resolver cache",
	})

	SchemaResolverAPICallDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "schema_resolver_api_call_duration_seconds",
		Help:    "Duration of API calls to fetch schemas",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
	})

	SchemaResolverSingleflightDeduplicatedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_singleflight_deduplicated_total",
		Help: "Total number of requests that were deduplicated by singleflight",
	})

	SchemaResolutionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "schema_resolver_errors_total",
		Help: "Total number of schema resolution errors",
	})
)
