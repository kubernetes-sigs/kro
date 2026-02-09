---
sidebar_position: 3
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
