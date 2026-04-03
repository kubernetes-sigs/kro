---
sidebar_position: 5
---

# Controller Tuning

kro has two main reconciliation loops: the RGD reconciler that processes ResourceGraphDefinitions, and the dynamic controller that manages instances. This page explains both and their tuning options.

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
| `config.instance.requeueInterval` | `3s` | Fixed delay for delayed instance requeues when kro is waiting for resources, readiness, or deletion to settle. Set to `0` to disable delayed requeues |

This setting is also available as the `--instance-requeue-interval` flag.

### Rate Limiting

The queue uses a combined rate limiter with two strategies:

1. **Exponential backoff** - Failed items are requeued with increasing delays
2. **Bucket rate limiter** - Limits overall event processing rate

These settings are only available via command-line flags:

| Flag | Default | Description |
|------|---------|-------------|
| `--dynamic-controller-rate-limiter-min-delay` | 200ms | Initial retry delay |
| `--dynamic-controller-rate-limiter-max-delay` | 1000s | Maximum retry delay |
| `--dynamic-controller-rate-limiter-rate-limit` | 10 | Events per second |
| `--dynamic-controller-rate-limiter-burst-limit` | 100 | Burst capacity |

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
make build-debug-image RELEASE_VERSION=v0.9.0
make publish-debug-image RELEASE_VERSION=v0.9.0
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
