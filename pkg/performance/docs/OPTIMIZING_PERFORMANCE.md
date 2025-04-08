# Optimizing KRO Performance

This document provides guidance on optimizing KRO performance based on benchmark results.

## General Optimization Strategies

### Resource Configuration

1. **Memory Allocation**: Configure appropriate memory limits based on workload size
   - Basic workloads: 256MB-512MB
   - Medium workloads: 512MB-1GB
   - Large workloads: 1GB+

2. **CPU Allocation**: Allocate CPU resources based on expected operations per second
   - Low throughput (<100 ops/sec): 0.5 CPU
   - Medium throughput (100-500 ops/sec): 1-2 CPUs
   - High throughput (500+ ops/sec): 2+ CPUs

3. **Worker Scaling**: Configure worker count based on available resources
   - Optimal worker count typically matches available CPU cores
   - Diminishing returns observed beyond 4-8 workers for most operations

### Operational Patterns

1. **Batch Operations**: Group operations when possible for better efficiency
   - List operations are more efficient than multiple Get operations
   - Create/update operations benefit from batching

2. **Watch vs. Poll**: Use Watch operations for events instead of frequent polling
   - Watch operations create less load on the system
   - Efficient for reactive patterns

3. **Resource Caching**: Implement appropriate caching strategies
   - Client-side caching reduces API calls
   - TTL-based caching balances freshness and performance

## CEL Expression Optimization

1. **Expression Complexity**: Optimize CEL expression complexity
   - Simple expressions evaluate ~20-30x faster than very complex ones
   - Avoid deeply nested expressions when possible

2. **Compilation Caching**: Cache compiled expressions for reuse
   - Parsing/compilation is expensive compared to evaluation
   - Reuse compiled expressions for similar patterns

3. **Selective Evaluation**: Evaluate expressions only when needed
   - Use short-circuit evaluation patterns
   - Filter resources early to reduce evaluation load

## ResourceGraph Optimization

1. **Graph Size Management**: Limit graph size for better performance
   - Performance decreases non-linearly with graph size
   - Large graphs (30+ nodes) require significantly more resources

2. **Traversal Optimization**: Optimize graph traversal patterns
   - Depth-first vs. breadth-first selection based on graph characteristics
   - Implement targeted traversal rather than full graph processing

3. **Relationship Indexing**: Index common relationship patterns
   - Build indexes for frequently traversed relationships
   - Cache common subgraphs for reuse

## Scalability Considerations

1. **Horizontal Scaling**: Consider horizontal scaling for high throughput requirements
   - Multiple KRO instances with sharded responsibility
   - Increased resilience through redundancy

2. **Workload Partitioning**: Partition workloads based on resource types or namespaces
   - Dedicated instances for high-frequency operations
   - Separation of read and write workflows

3. **Resource Limits**: Implement resource limits to prevent overload
   - Rate limiting for API operations
   - Backpressure mechanisms for high-load scenarios

## Performance Troubleshooting

When facing performance issues:

1. **Metric Analysis**: Analyze benchmark metrics to identify bottlenecks
   - Compare against baseline performance
   - Look for outliers in specific operation types

2. **Resource Profiling**: Profile resource usage during operation
   - CPU profiling to identify hot spots
   - Memory profiling to detect leaks or excessive allocations

3. **Operation Distribution**: Analyze distribution of operations
   - Identify imbalanced workloads
   - Adjust resource allocation based on actual usage patterns

## Configuration Examples

### Small Deployment (Development/Testing)

```yaml
resources:
  limits:
    memory: 512Mi
    cpu: 0.5
  requests:
    memory: 256Mi
    cpu: 0.2
```

### Medium Deployment (Production/Small Cluster)

```yaml
resources:
  limits:
    memory: 1Gi
    cpu: 1
  requests:
    memory: 512Mi
    cpu: 0.5
```

### Large Deployment (Large Cluster/High Throughput)

```yaml
resources:
  limits:
    memory: 2Gi
    cpu: 2
  requests:
    memory: 1Gi
    cpu: 1
```

## Performance Benchmarks

Use the performance testing framework to establish baseline metrics for your specific environment:

```bash
./pkg/performance/run_performance_tests.sh --duration=300s --workers=4 --resources=500
```

Compare results across different configurations to find optimal settings.
