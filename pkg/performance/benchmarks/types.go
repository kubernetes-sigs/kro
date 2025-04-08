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