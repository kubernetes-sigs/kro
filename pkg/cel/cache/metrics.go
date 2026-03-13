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

package cache

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// BuilderCache metrics — long-lived, cross-RGD cache.

	builderCacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cel_cache_builder_hits_total",
			Help: "Total number of builder cache hits",
		},
		[]string{"cache_type"},
	)
	builderCacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cel_cache_builder_misses_total",
			Help: "Total number of builder cache misses",
		},
		[]string{"cache_type"},
	)
	builderCacheSize = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cel_cache_builder_size",
			Help: "Current number of entries in the builder cache",
		},
		[]string{"cache_type"},
	)

	// SessionCache metrics — short-lived, per-RGD-build cache.

	sessionCacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cel_cache_session_hits_total",
			Help: "Total number of session cache hits",
		},
		[]string{"cache_type"},
	)
	sessionCacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "cel_cache_session_misses_total",
			Help: "Total number of session cache misses",
		},
		[]string{"cache_type"},
	)
	sessionCacheASTReuseTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "cel_cache_session_ast_reuse_total",
			Help: "Total number of checked ASTs reused during program compilation (skipping parse+check)",
		},
	)
)

func init() {
	metrics.Registry.MustRegister(
		builderCacheHitsTotal,
		builderCacheMissesTotal,
		builderCacheSize,
		sessionCacheHitsTotal,
		sessionCacheMissesTotal,
		sessionCacheASTReuseTotal,
	)
}
