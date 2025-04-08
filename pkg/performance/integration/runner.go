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

package integration

import (
	"fmt"
	"time"

	"github.com/kro-run/kro/pkg/performance/benchmarks"
)

// PerformanceTestRunner runs performance tests in integration mode
type PerformanceTestRunner struct {
	config *benchmarks.BenchmarkConfig
}

// NewPerformanceTestRunner creates a new performance test runner for integration tests
func NewPerformanceTestRunner(config *benchmarks.BenchmarkConfig) *PerformanceTestRunner {
	return &PerformanceTestRunner{
		config: config,
	}
}

// RunTests executes the performance test suite
func (r *PerformanceTestRunner) RunTests() (*benchmarks.BenchmarkResults, error) {
	suite := benchmarks.NewBenchmarkSuite(r.config)
	
	// Add benchmarks
	suite.AddBenchmark(benchmarks.NewCRUDTest())
	suite.AddBenchmark(benchmarks.NewCELTest())
	suite.AddBenchmark(benchmarks.NewResourceGraphTest())
	
	// Run the suite
	fmt.Println("Running integration performance tests...")
	start := time.Now()
	
	results, err := suite.Run()
	if err != nil {
		return nil, fmt.Errorf("error running benchmark suite: %v", err)
	}
	
	elapsed := time.Since(start)
	fmt.Printf("Tests completed in %s\n", elapsed)
	
	return results, nil
}
