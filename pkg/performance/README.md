# KRO Performance Framework

This directory contains the KRO Performance Framework, a comprehensive testing infrastructure for measuring KRO (Kubernetes Resource Orchestrator) performance metrics.

## Overview

The framework is designed to benchmark and analyze the performance of various KRO components:

1. **CRUD Operations**: Create, Read, Update, Delete operations on KRO resources
2. **CEL Expressions**: Common Expression Language evaluation with varying complexity levels
3. **ResourceGraph Operations**: Graph operations with different sizes and complexities

## Getting Started

### Prerequisites

- Go 1.21 or later
- Kubernetes cluster (optional for simulation mode)

### Installation

The framework is part of the KRO repository. Simply clone the repository:

```bash
git clone https://github.com/kro-run/kro.git
cd kro/pkg/performance
```

### Building

Build the framework:

```bash
go build -o kro-perf main.go
```

## Usage

### Quick Start

To run a quick demo of simulated performance results:

```bash
./run_performance_tests.sh --demo
```

### Running Benchmarks

To run all benchmarks:

```bash
./kro-perf benchmark --simulation --duration 30s --workers 4 --resources 100
```

Or you can use the shell script with similar options:

```bash
./run_performance_tests.sh --duration 30s --workers 4 --resources 100
```

### Running Specific Benchmarks

Run only CRUD benchmarks:

```bash
./kro-perf benchmark --type crud --simulation
```

Run only CEL expression benchmarks:

```bash
./kro-perf benchmark --type cel --complexity all --simulation
```

Run only ResourceGraph benchmarks:

```bash
./kro-perf benchmark --type resourcegraph --graph-complexity all --simulation
```

### Analysis and Visualization

Analyze benchmark results:

```bash
./kro-perf analyze --input ./results/all_results.json --output ./results/analysis.json
```

Generate visualizations:

```bash
./kro-perf visualize --input ./results/analysis.json --output ./results/visualizations --charts all
```

## Command Line Options

### Benchmark Command

| Option | Description | Default |
|--------|-------------|---------|
| `--type` | Benchmark type: crud, cel, resourcegraph, all | all |
| `--duration` | Duration of each benchmark | 60s |
| `--workers` | Number of concurrent workers | 4 |
| `--resources` | Number of resources for CRUD tests | 100 |
| `--namespace` | Kubernetes namespace | default |
| `--kubeconfig` | Path to kubeconfig file | |
| `--simulation` | Run in simulation mode | true |
| `--complexity` | CEL expression complexity: simple, medium, complex, very-complex, all | all |
| `--graph-complexity` | Graph complexity: small, medium, large, all | all |
| `--nodes` | Number of nodes for large graph tests | 30 |
| `--edges` | Number of edges for large graph tests | 45 |
| `--output` | Output directory for results | ./results |
| `--verbose` | Enable verbose output | false |

### Analyze Command

| Option | Description | Default |
|--------|-------------|---------|
| `--input` | Input results file to analyze | (required) |
| `--output` | Output file for analysis | (required) |
| `--baseline` | Baseline results for comparison | |
| `--format` | Output format: json, yaml, text | json |
| `--compare-to-last` | Compare to last run | false |

### Visualize Command

| Option | Description | Default |
|--------|-------------|---------|
| `--input` | Input analysis file to visualize | (required) |
| `--output` | Output directory for visualizations | (required) |
| `--charts` | Chart types to generate: bar, line, heatmap, all | all |

## Architecture

See the [OVERVIEW.md](./OVERVIEW.md) file for details on the architecture of the performance framework.

## Running Tests

Run the Go benchmarks:

```bash
go test -bench=. ./benchmarks/...
```

Run Go unit tests:

```bash
go test ./...
```
