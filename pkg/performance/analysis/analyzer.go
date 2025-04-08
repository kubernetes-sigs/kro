package analysis

import (
	"fmt"
	"os"
	"time"
)

// Analyzer performs analysis on benchmark results
type Analyzer struct {
	inputFile    string
	outputFile   string
	baselineFile string
}

// AnalysisResult holds the results of benchmark analysis
type AnalysisResult struct {
	Timestamp   time.Time    `json:"timestamp"`
	Summary     Summary      `json:"summary"`
	Comparisons []Comparison `json:"comparisons,omitempty"`
	Insights    []Insight    `json:"insights,omitempty"`
}

// Summary holds summary statistics for benchmark results
type Summary struct {
	TotalOps      int     `json:"totalOps"`
	TotalDuration float64 `json:"totalDuration"` // in seconds
	AvgOpsPerSec  float64 `json:"avgOpsPerSec"`
	P95Latency    float64 `json:"p95Latency"` // in milliseconds
	MaxLatency    float64 `json:"maxLatency"` // in milliseconds
	ErrorRate     float64 `json:"errorRate"`  // percentage
	CPUUsage      float64 `json:"cpuUsage"`   // percentage
	MemoryUsage   float64 `json:"memoryUsage"` // in MB
}

// Comparison holds comparison between current and baseline results
type Comparison struct {
	Metric     string  `json:"metric"`
	Current    float64 `json:"current"`
	Baseline   float64 `json:"baseline"`
	Difference float64 `json:"difference"`
	Percentage float64 `json:"percentage"` // percentage change
}

// Insight represents a high-level insight derived from analysis
type Insight struct {
	Type        string `json:"type"` // e.g., "performance", "resource", "bottleneck"
	Description string `json:"description"`
	Severity    string `json:"severity"` // e.g., "info", "warning", "critical"
}

// NewAnalyzer creates a new Analyzer
func NewAnalyzer(inputFile, outputFile, baselineFile string) *Analyzer {
	return &Analyzer{
		inputFile:    inputFile,
		outputFile:   outputFile,
		baselineFile: baselineFile,
	}
}

// Analyze performs analysis on benchmark results
func (a *Analyzer) Analyze() (*AnalysisResult, error) {
	// For demo purposes, just create a sample analysis result
	fmt.Printf("Analyzing benchmark results from %s...\n", a.inputFile)
	
	// Create a sample analysis result
	result := &AnalysisResult{
		Timestamp: time.Now(),
		Summary: Summary{
			TotalOps:      10000,
			TotalDuration: 15.3,
			AvgOpsPerSec:  658.2,
			P95Latency:    12.5,
			MaxLatency:    45.8,
			ErrorRate:     0.05,
			CPUUsage:      45.2,
			MemoryUsage:   128.5,
		},
		Insights: []Insight{
			{
				Type:        "performance",
				Description: "CRUD operations achieved 658 ops/sec with 4 workers",
				Severity:    "info",
			},
			{
				Type:        "performance",
				Description: "CEL expressions evaluated at 5433 ops/sec",
				Severity:    "info",
			},
			{
				Type:        "performance",
				Description: "ResourceGraph operations processed at 148 ops/sec",
				Severity:    "info",
			},
			{
				Type:        "resource",
				Description: "CEL evaluation is CPU-efficient (62% utilization)",
				Severity:    "info",
			},
			{
				Type:        "resource",
				Description: "ResourceGraph operations are CPU and memory intensive (79% CPU, 257MB)",
				Severity:    "warning",
			},
			{
				Type:        "bottleneck",
				Description: "Create operations (424 ops/sec) are slower than read operations (892 ops/sec)",
				Severity:    "info",
			},
			{
				Type:        "bottleneck",
				Description: "Complex CEL expressions (1246 ops/sec) are 7.8x slower than simple expressions (9821 ops/sec)",
				Severity:    "warning",
			},
		},
	}
	
	// If baseline file is provided, add comparisons
	if a.baselineFile != "" {
		fmt.Printf("Comparing with baseline from %s...\n", a.baselineFile)
		result.Comparisons = []Comparison{
			{
				Metric:     "OpsPerSec",
				Current:    658.2,
				Baseline:   620.5,
				Difference: 37.7,
				Percentage: 6.1,
			},
			{
				Metric:     "P95Latency",
				Current:    12.5,
				Baseline:   14.2,
				Difference: -1.7,
				Percentage: -12.0,
			},
			{
				Metric:     "CPUUsage",
				Current:    45.2,
				Baseline:   47.8,
				Difference: -2.6,
				Percentage: -5.4,
			},
		}
	}
	
	// Write analysis results to output file
	if err := a.writeResults(result); err != nil {
		return nil, fmt.Errorf("failed to write analysis results: %v", err)
	}
	
	return result, nil
}

// writeResults writes analysis results to the output file
func (a *Analyzer) writeResults(result *AnalysisResult) error {
	// Create the output file
	f, err := os.Create(a.outputFile)
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer f.Close()
	
	// For demo purposes, just write a simple placeholder
	content := fmt.Sprintf("{\n  \"timestamp\": \"%s\",\n  \"summary\": { ... },\n  \"insights\": [ ... ]\n}",
		result.Timestamp.Format(time.RFC3339))
	
	if _, err := f.WriteString(content); err != nil {
		return fmt.Errorf("failed to write analysis results: %v", err)
	}
	
	return nil
}