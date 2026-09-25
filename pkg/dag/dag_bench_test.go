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

package dag

import (
	"fmt"
	"testing"
)

// benchID returns a deterministic vertex ID so that benchmark runs compare.
func benchID(i int) string { return fmt.Sprintf("v%06d", i) }

// BenchmarkAddDependenciesConstruct builds a DAG the way callers do: add all
// vertices, then call AddDependencies once per vertex. In tree8, vertex j
// depends on j/8, so each walk is short. chain is the worst case: each walk
// crosses the full chain.
func BenchmarkAddDependenciesConstruct(b *testing.B) {
	depsOf := map[string]func(j int) []string{
		"tree8": func(j int) []string {
			if j == 0 {
				return nil
			}
			return []string{benchID(j / 8)}
		},
		"chain": func(j int) []string {
			if j == 0 {
				return nil
			}
			return []string{benchID(j - 1)}
		},
	}
	sizes := map[string][]int{
		"tree8": {128, 2048, 8192},
		"chain": {128, 512, 2048},
	}

	for _, shape := range []string{"tree8", "chain"} {
		for _, n := range sizes[shape] {
			b.Run(fmt.Sprintf("%s/n=%d", shape, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					d := NewDirectedAcyclicGraph[string]()
					for j := range n {
						if err := d.AddVertex(benchID(j), j); err != nil {
							b.Fatalf("AddVertex: %v", err)
						}
					}
					b.StartTimer()
					for j := range n {
						if deps := depsOf[shape](j); deps != nil {
							if err := d.AddDependencies(benchID(j), deps); err != nil {
								b.Fatalf("AddDependencies: %v", err)
							}
						}
					}
				}
			})
		}
	}
}

// BenchmarkAddDependenciesRejectCycle measures the error path in a tree8
// graph of 2048 vertices.
func BenchmarkAddDependenciesRejectCycle(b *testing.B) {
	const n = 2048
	d := NewDirectedAcyclicGraph[string]()
	for j := range n {
		if err := d.AddVertex(benchID(j), j); err != nil {
			b.Fatalf("AddVertex: %v", err)
		}
	}
	for j := 1; j < n; j++ {
		if err := d.AddDependencies(benchID(j), []string{benchID(j / 8)}); err != nil {
			b.Fatalf("AddDependencies: %v", err)
		}
	}

	// v0 is reachable from v2047 through 255, 31 and 3.
	b.ReportAllocs()
	for b.Loop() {
		if err := d.AddDependencies(benchID(0), []string{benchID(2047)}); err == nil {
			b.Fatal("expected cycle error")
		}
	}
}
