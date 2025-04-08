// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package benchmarks

import "time"

// BenchmarkResult represents the result of a benchmark run
type BenchmarkResult struct {
	Name           string        `json:"name"`
	Type           string        `json:"type"`
	Duration       time.Duration `json:"duration"`
	OperationsRun  int           `json:"operationsRun"`
	OperationsPerSecond float64       `json:"operationsPerSecond"`
	AvgLatency     float64       `json:"avgLatency"`       // milliseconds
	P95Latency     float64       `json:"p95Latency"`       // milliseconds
	P99Latency     float64       `json:"p99Latency"`       // milliseconds
	MinLatency     float64       `json:"minLatency"`       // milliseconds
	MaxLatency     float64       `json:"maxLatency"`       // milliseconds
	ErrorCount     int           `json:"errorCount"`
	ErrorRate      float64       `json:"errorRate"`        // percentage
	CPUUsage       float64       `json:"cpuUsage"`         // percentage
	MemoryUsage    float64       `json:"memoryUsage"`      // MB
}

// SuiteConfig holds configuration for the benchmark suite
type SuiteConfig struct {
	Duration       time.Duration
	Workers        int
	ResourceCount  int
	SimulationMode bool
	Namespace      string
	KubeConfig     string
	Nodes          int // For resourcegraph benchmarks
	Edges          int // For resourcegraph benchmarks
}

// BenchmarkSuite runs benchmarks for KRO
type BenchmarkSuite struct {
	config *SuiteConfig
}

// NewBenchmarkSuite creates a new benchmark suite
func NewBenchmarkSuite(config *SuiteConfig) *BenchmarkSuite {
	return &BenchmarkSuite{
		config: config,
	}
}