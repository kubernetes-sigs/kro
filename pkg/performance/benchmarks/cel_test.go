package benchmarks

import (
	"context"
	"testing"
	"time"
)

// BenchmarkCELExpressions runs benchmark tests for CEL expressions
func BenchmarkCELExpressions(b *testing.B) {
	// Skip in short mode
	if testing.Short() {
		b.Skip("skipping test in short mode.")
	}

	// Create a BenchmarkSuite with a specified configuration
	suite := NewBenchmarkSuite(&SuiteConfig{
		Duration:       1 * time.Second,
		Workers:        4,
		ResourceCount:  100,
		SimulationMode: true,
		Namespace:      "test",
	})

	// Run the benchmark
	b.Run("Simple", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkCELSimple(b, suite)
		}
	})

	b.Run("Medium", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkCELMedium(b, suite)
		}
	})

	b.Run("Complex", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkCELComplex(b, suite)
		}
	})

	b.Run("VeryComplex", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkCELVeryComplex(b, suite)
		}
	})
}

// benchmarkCELSimple benchmarks simple CEL expressions
// Examples: a == 1, x > 10, name.startsWith("pod-")
func benchmarkCELSimple(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate CEL evaluation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%1000 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// Here we'd actually call the CEL engine to evaluate an expression
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(100 * time.Nanosecond) // Simple expressions are very fast
	}
}

// benchmarkCELMedium benchmarks medium complexity CEL expressions
// Examples: x > 0 && x < 100, (a.b == 1) || (a.c > 2), has(items) && size(items) > 0
func benchmarkCELMedium(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate CEL evaluation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%500 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(200 * time.Nanosecond) // Medium expressions take a bit longer
	}
}

// benchmarkCELComplex benchmarks complex CEL expressions
// Examples: sum(items.map(i, i.value)) > 100, 
//           all(resources, r, r.status == "ready") && size(resources) > 3,
//           items.filter(i, i.value > 10).size() > 5
func benchmarkCELComplex(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate CEL evaluation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%200 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(800 * time.Nanosecond) // Complex expressions are slower
	}
}

// benchmarkCELVeryComplex benchmarks very complex CEL expressions
// Examples: nested map-reduce operations, multiple quantified expressions,
//           complex string operations, deeply nested traversals
func benchmarkCELVeryComplex(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate CEL evaluation
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(1300 * time.Nanosecond) // Very complex expressions are the slowest
	}
}