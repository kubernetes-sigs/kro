---
sidebar_position: 3
---

# Controller Tuning

kro has three reconciliation loops: the RGD reconciler that processes ResourceGraphDefinitions, the dynamic controller that manages instances, and the Graph controller that reconciles [Graphs](../concepts/graph/01-overview.md). This page explains them and their tuning options, along with settings shared by the composition engine underneath.

## RGD Reconciler

The RGD reconciler watches ResourceGraphDefinition resources. When you create or update an RGD, it:

1. Validates the schema and resource templates (see [Static Type Checking](../concepts/rgd/05-static-type-checking.md))
2. Creates or updates the generated CRD
3. Registers the instance handler with the dynamic controller

| Setting | Default | Description |
|---------|---------|-------------|
| `config.resourceGraphDefinitionConcurrentReconciles` | 1 | Parallel RGD reconciles |

Increase this if you're creating many RGDs simultaneously:

```yaml
config:
  resourceGraphDefinitionConcurrentReconciles: 3
```

## Dynamic Controller

The dynamic controller is a custom architecture designed for managing multiple resource types at runtime. Unlike traditional controllers that watch fixed resources, it adapts dynamically - when you create an RGD, it registers new watches without requiring restarts.

### Architecture

```
+----------------------------------------------------------+
|                    Dynamic Controller                    |
|                                                          |
|  +--------------+  +--------------+  +--------------+    |
|  |   Informer   |  |   Informer   |  |   Informer   |    |
|  |   (WebApp)   |  | (Deployment) |  |  (Service)   |    |
|  +------+-------+  +------+-------+  +------+-------+    |
|         |                 |                 |            |
|         +-----------------+-----------------+            |
|                           |                              |
|                           v                              |
|                  +----------------+                      |
|                  |  Shared Queue  |                      |
|                  +-------+--------+                      |
|                          |                               |
|           +--------------+--------------+                |
|           |              |              |                |
|           v              v              v                |
|      +--------+    +--------+    +--------+              |
|      |Worker 1|    |Worker 2|    |Worker N|              |
|      +--------+    +--------+    +--------+              |
+----------------------------------------------------------+
```

The controller is designed around a few core principles:

- **Single shared queue** - All resource events flow through one rate-limited queue, preventing any single RGD from overwhelming the system
- **Lazy informers** - Informers are created on-demand when an RGD is registered and stopped when deregistered
- **Parent-child watches** - The controller watches both instances (parent) and their managed resources (children). Child events trigger parent reconciliation via labels
- **Metadata-only watches** - The dynamic controller only fetches metadata, reducing memory overhead

:::note
kro is in active development. This architecture may evolve - for example, the shared queue could be replaced with per-RGD queues in future versions.
:::

### Concurrency

| Setting | Default | Description |
|---------|---------|-------------|
| `config.dynamicControllerConcurrentReconciles` | 1 | Workers processing instances |

```yaml
config:
  dynamicControllerConcurrentReconciles: 10
```

More workers increase throughput but also increase concurrent API server load.

### Resync and Retries

| Setting | Default | Description |
|---------|---------|-------------|
| `config.dynamicControllerDefaultResyncPeriod` | 36000 | Seconds between full resyncs (10 hours) |
| `config.dynamicControllerDefaultQueueMaxRetries` | 20 | Retries before dropping an item |

The resync period triggers reconciliation for all resources periodically, even without changes. This catches any drift that might have been missed.

### Instance Requeues

| Setting | Default | Description |
|---------|---------|-------------|
| `config.instance.requeueInterval` | `3s` | Initial delay for delayed instance requeues when kro is waiting for resources, readiness, or deletion to settle. Set to `0` to disable delayed requeues |

This setting is also available as the `--instance-requeue-interval` flag.

When an instance is waiting on cluster state that is not ready yet (an external
reference that does not exist, a `readyWhen` that is still false), consecutive
requeues back off exponentially from this interval, doubling each time up to a
cap of five minutes. The backoff resets as soon as a reconcile makes progress.
Changes to the resources the instance manages or reads still trigger an
immediate reconcile through kro's watches.

### Rate Limiting

The queue uses a combined rate limiter with two strategies:

1. **Exponential backoff** - Failed items are requeued with increasing delays
2. **Bucket rate limiter** - Limits overall event processing rate

| Setting | Flag | Default | Description |
|---------|------|---------|-------------|
| `config.dynamicControllerRateLimiterMinDelay` | `--dynamic-controller-rate-limiter-min-delay` | 200ms | Initial retry delay |
| `config.dynamicControllerRateLimiterMaxDelay` | `--dynamic-controller-rate-limiter-max-delay` | 1000s | Maximum retry delay |
| `config.dynamicControllerRateLimiterRateLimit` | `--dynamic-controller-rate-limiter-rate-limit` | 10 | Events per second |
| `config.dynamicControllerRateLimiterBurstLimit` | `--dynamic-controller-rate-limiter-burst-limit` | 100 | Burst capacity |

## Graph Controller

The Graph controller reconciles `Graph` resources. It runs only when the
`GraphKind` [feature gate](./02-feature-gates.md#graphkind) is enabled.

| Setting | Default | Description |
|---------|---------|-------------|
| `config.graphConcurrentReconciles` | 1 | Parallel Graph reconciles |

Also available as the `--graph-concurrent-reconciles` flag. Within one Graph,
nodes are applied serially in dependency order; this setting controls how many
distinct Graphs reconcile at once.

Two flags have no Helm value and are set by the chart automatically:

| Flag | Description |
|------|-------------|
| `--controller-namespace` | The namespace the kro controller runs in |
| `--controller-service-account` | The kro controller's own ServiceAccount name |

Together they let the Graph controller refuse a Graph that would impersonate
kro's own identity. When `GraphKind` is enabled and either is unset, the
controller exits at startup. If you deploy kro without the chart, pass both.

## Composition Engine

These settings apply to both instance reconciliation and Graph reconciliation.

| Setting | Default | Description |
|---------|---------|-------------|
| `config.applyConcurrency` | 20 | Maximum concurrent server-side apply writes for the items of a single `forEach` collection |
| `config.rgd.maxCollectionSize` | 1000 | Maximum items a `forEach` expansion may produce |
| `config.rgd.maxCollectionDimensionSize` | 10 | Maximum `forEach` dimensions on one resource |
| `config.celCostLimit` | 0 | Cost budget for evaluating a single CEL expression. `0` disables the limit |

The flag equivalents are `--apply-concurrency`, `--rgd-max-collection-size`,
`--rgd-max-collection-dimension-size`, and `--cel-cost-limit`.

`celCostLimit` bounds the work one expression may do, using CEL's cost model.
When set, an expression that exceeds the budget fails evaluation. Leave it at
`0` unless you need a hard ceiling on the evaluation time of any single
expression.

## API Server Communication

These settings control how kro communicates with the Kubernetes API server:

| Setting | Default | Description |
|---------|---------|-------------|
| `config.clientQps` | 100 | Maximum queries per second |
| `config.clientBurst` | 150 | Burst requests before throttling |

Increase for larger clusters:

```yaml
config:
  clientQps: 200
  clientBurst: 300
```

## pprof Profiling

For performance testing and troubleshooting, kro provides a debug image variant with [pprof](https://pkg.go.dev/net/http/pprof) profiling enabled.

:::warning
The debug image exposes sensitive performance data through pprof endpoints. **Do not use in production environments.**
:::

### Enable pprof in Helm

Enable pprof in your Helm values:

```yaml
debug:
  pprof:
    enabled: true    # Uses the -debug tagged image
    port: 6060       # Port for the pprof HTTP server
    service:
      enabled: true  # Create a Service for port-forwarding
```

This switches the chart to the `-debug` image tag and configures the controller to serve pprof on the configured port.

### Build the pprof Image

Use the dedicated Make targets when building or publishing the pprof-enabled image:

```bash
make build-debug-image RELEASE_VERSION=v0.9.4
make publish-debug-image RELEASE_VERSION=v0.9.4
```

If you deploy with `image.ko=true` or use `ko apply` directly, build with `GOFLAGS="-tags=pprof"` so the pprof handlers are compiled into the controller binary.

### Collect a Profile

If you enabled the pprof Service, port-forward it locally:

```bash
kubectl -n kro-system port-forward service/<helm-release>-pprof 6060:6060
```

If you left the Service disabled, port-forward the controller Pod instead.

Capture a CPU profile while reproducing the issue:

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/profile?seconds=30
```

Inspect heap growth when chasing memory pressure:

```bash
go tool pprof http://127.0.0.1:6060/debug/pprof/heap
```

Inside the `pprof` shell, start with `top`, `top -cum`, and `list <function>` to find the hottest code paths.

### What to Look For

- High CPU time in reconciliation hot paths such as graph construction, CEL evaluation, or repeated object conversion.
- Large retained heap in informer caches, unstructured object copies, or repeated allocations inside reconcile loops.
- Excess time spent in Kubernetes client calls, which can indicate that `config.clientQps` and `config.clientBurst` are too low for the cluster size.
