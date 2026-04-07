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
	"context"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	clientmetrics "k8s.io/client-go/tools/metrics"
)

var (
	requestLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_request_duration_seconds",
		Help:    "Request latency in seconds, partitioned by verb.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
	}, []string{"verb"})

	rateLimiterLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_rate_limiter_duration_seconds",
		Help:    "Client-side rate limiter latency in seconds, partitioned by verb.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 16),
	}, []string{"verb"})

	requestSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_request_size_bytes",
		Help:    "Request size in bytes, partitioned by verb.",
		Buckets: prometheus.ExponentialBuckets(64, 2, 16),
	}, []string{"verb"})

	responseSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "rest_client_response_size_bytes",
		Help:    "Response size in bytes, partitioned by verb.",
		Buckets: prometheus.ExponentialBuckets(64, 2, 16),
	}, []string{"verb"})

	requestRetry = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rest_client_request_retries_total",
		Help: "Total number of request retries, partitioned by status code and method.",
	}, []string{"code", "method"})
)

// registerClientGoAdapters wires up the client-go metrics adapters.
// controller-runtime v0.16+ removed these histograms from its client-go
// adapter. The one-shot clientmetrics.Register() has already been consumed
// by controller-runtime, so we assign the globals directly.
func registerClientGoAdapters() {
	clientmetrics.RequestLatency = &latencyAdapter{metric: requestLatency}
	clientmetrics.RateLimiterLatency = &latencyAdapter{metric: rateLimiterLatency}
	clientmetrics.RequestSize = &sizeAdapter{metric: requestSize}
	clientmetrics.ResponseSize = &sizeAdapter{metric: responseSize}
	clientmetrics.RequestRetry = &retryAdapter{metric: requestRetry}
}

type latencyAdapter struct {
	metric *prometheus.HistogramVec
}

func (l *latencyAdapter) Observe(_ context.Context, verb string, _ url.URL, latency time.Duration) {
	l.metric.WithLabelValues(verb).Observe(latency.Seconds())
}

type sizeAdapter struct {
	metric *prometheus.HistogramVec
}

func (s *sizeAdapter) Observe(_ context.Context, verb string, _ string, size float64) {
	s.metric.WithLabelValues(verb).Observe(size)
}

type retryAdapter struct {
	metric *prometheus.CounterVec
}

func (r *retryAdapter) IncrementRetry(_ context.Context, code string, method string, _ string) {
	r.metric.WithLabelValues(code, method).Inc()
}
