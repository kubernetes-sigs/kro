package benchmarks

import (
	"context"
	"testing"
	"time"
)

// BenchmarkCRUDOperations runs benchmark tests for CRUD operations
func BenchmarkCRUDOperations(b *testing.B) {
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
	b.Run("Create", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkCreate(b, suite)
		}
	})

	b.Run("Read", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkRead(b, suite)
		}
	})

	b.Run("Update", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkUpdate(b, suite)
		}
	})

	b.Run("Delete", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			benchmarkDelete(b, suite)
		}
	})
}

// benchmarkCreate benchmarks create operations
func benchmarkCreate(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate create operations
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// Here we'd actually call the KRO API to create a resource
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(1 * time.Microsecond)
	}
}

// benchmarkRead benchmarks read operations
func benchmarkRead(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate read operations
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// Here we'd actually call the KRO API to read a resource
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(1 * time.Microsecond)
	}
}

// benchmarkUpdate benchmarks update operations
func benchmarkUpdate(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate update operations
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// Here we'd actually call the KRO API to update a resource
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(1 * time.Microsecond)
	}
}

// benchmarkDelete benchmarks delete operations
func benchmarkDelete(b *testing.B, suite *BenchmarkSuite) {
	// Create a context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Simulate delete operations
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 && ctx.Err() != nil {
			b.Fatalf("context cancelled: %v", ctx.Err())
		}
		// Here we'd actually call the KRO API to delete a resource
		// For demo purposes, we'll just sleep to simulate the operation
		time.Sleep(1 * time.Microsecond)
	}
}