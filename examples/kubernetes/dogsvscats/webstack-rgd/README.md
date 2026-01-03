# WebStack RGD - Unified Web Application Stack

A unified Resource Graph Definition that can deploy Vote, Result, or Worker applications based on configuration.

## Architecture

This example demonstrates a **polymorphic RGD pattern** where a single `WebStack` RGD can instantiate different application types (Vote, Result, or Worker) based on the `appType` parameter. Each application type has its own underlying RGD but is exposed through a unified interface.

```
┌─────────────────────────────────────────┐
│          WebStack (Unified RGD)         │
│                                         │
│  ┌─────────────────────────────────┐   │
│  │  appType: "vote"                │   │
│  │  ├─> VoteWebStack RGD           │   │
│  │  │   ├─ Deployment              │   │
│  │  │   ├─ Service (optional)      │   │
│  │  │   └─ Ingress (optional)      │   │
│  └─────────────────────────────────┘   │
│                                         │
│  ┌─────────────────────────────────┐   │
│  │  appType: "result"              │   │
│  │  ├─> ResultWebStack RGD         │   │
│  │  │   ├─ Deployment              │   │
│  │  │   ├─ Service (optional)      │   │
│  │  │   └─ Ingress (optional)      │   │
│  └─────────────────────────────────┘   │
│                                         │
│  ┌─────────────────────────────────┐   │
│  │  appType: "worker"              │   │
│  │  ├─> WorkerWebStack RGD         │   │
│  │  │   └─ Deployment              │   │
│  └─────────────────────────────────┘   │
└─────────────────────────────────────────┘
```

## Components

### Component RGDs

1. **vote-rgd.yaml** - VoteWebStack
   - Deployment for vote application
   - Optional Service
   - Optional Ingress
   - ElastiCache integration flag

2. **result-rgd.yaml** - ResultWebStack
   - Deployment for result application
   - Optional Service
   - Optional Ingress
   - PostgreSQL integration flag

3. **worker-rgd.yaml** - WorkerWebStack
   - Deployment for worker application
   - Optional Service (typically disabled)

### Unified RGD

**web-stack-rgd.yaml** - WebStack
- Single unified interface for all three application types
- Uses `appType` parameter to determine which component RGD to instantiate
- Conditional resource inclusion based on application type

## Usage

### 1. Apply the Component RGDs

First, apply the three component RGDs:

```bash
kubectl apply -f vote-rgd.yaml
kubectl apply -f result-rgd.yaml
kubectl apply -f worker-rgd.yaml
```

### 2. Apply the Unified WebStack RGD

```bash
kubectl apply -f web-stack-rgd.yaml
```

### 3. Verify RGDs are Active

```bash
kubectl get rgd
```

Expected output:
```
NAME                        APIVERSION   KIND              STATE    AGE
vote-webstack.kro.run       v1alpha1     VoteWebStack      Active   1m
result-webstack.kro.run     v1alpha1     ResultWebStack    Active   1m
worker-webstack.kro.run     v1alpha1     WorkerWebStack    Active   1m
webstack.kro.run            v1alpha1     WebStack          Active   1m
```

### 4. Create Instances

#### Vote Instance (with Ingress and ElastiCache)

```bash
kubectl apply -f instances/vote-instance.yaml
```

This creates:
- Vote Deployment
- Service (enabled)
- Ingress (enabled)
- ElastiCache integration (enabled)

#### Result Instance (with Ingress and PostgreSQL)

```bash
kubectl apply -f instances/result-instance.yaml
```

This creates:
- Result Deployment
- Service (enabled)
- Ingress (enabled)
- PostgreSQL integration (enabled)

#### Worker Instance (Service disabled)

```bash
kubectl apply -f instances/worker-instance.yaml
```

This creates:
- Worker Deployment only
- Service (disabled)

### 5. Verify Instances

```bash
kubectl get webstack
```

## Configuration Options

### Common Parameters

- `name`: Name of the application
- `namespace`: Kubernetes namespace (default: "default")
- `appType`: Type of application ("vote", "result", or "worker")
- `image`: Container image to use
- `replicas`: Number of replicas

### Vote-Specific Parameters

- `redisEndpoint`: Redis endpoint for caching
- `elasticache.enabled`: Enable ElastiCache integration

### Result-Specific Parameters

- `postgresEndpoint`: PostgreSQL endpoint
- `postgresSecretName`: Name of secret containing database credentials
- `postgres.enabled`: Enable PostgreSQL integration

### Worker-Specific Parameters

- `redisEndpoint`: Redis endpoint
- `postgresEndpoint`: PostgreSQL endpoint
- `postgresSecretName`: Database credentials secret

### Feature Flags

- `service.enabled`: Create a Kubernetes Service (default: true for vote/result, false for worker)
- `ingress.enabled`: Create an Ingress resource (default: false)

## Benefits

1. **Unified Interface**: Single RGD for all application types
2. **Type Safety**: Each application type uses its own specialized RGD
3. **Flexible Configuration**: Enable/disable features per instance
4. **Reusable Components**: Component RGDs can be used independently
5. **Clear Separation**: Infrastructure concerns separated by application type

## Example: Complete Stack

To deploy a complete voting application stack:

```bash
# Apply all RGDs
kubectl apply -f vote-rgd.yaml
kubectl apply -f result-rgd.yaml
kubectl apply -f worker-rgd.yaml
kubectl apply -f web-stack-rgd.yaml

# Deploy all three application types
kubectl apply -f instances/vote-instance.yaml
kubectl apply -f instances/result-instance.yaml
kubectl apply -f instances/worker-instance.yaml
```

## Clean Up

Remove instances:
```bash
kubectl delete -f instances/
```

Remove RGDs:
```bash
kubectl delete rgd webstack.kro.run
kubectl delete rgd vote-webstack.kro.run result-webstack.kro.run worker-webstack.kro.run
```

## Notes

- Update the endpoint values in instance files with your actual infrastructure endpoints
- The `appType` parameter is required and must be one of: "vote", "result", or "worker"
- Each instance creates resources based on its specific application type and enabled features
