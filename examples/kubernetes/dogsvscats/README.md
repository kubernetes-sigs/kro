# Dogs vs Cats - Kubernetes Application Components

Kubernetes application components for the Dogs vs Cats voting system. These cloud-agnostic RGDs can be deployed standalone or composed into larger application stacks.

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Dogs vs Cats Application                      │
│                                                                  │
│  ┌──────────────┐                         ┌──────────────┐      │
│  │   Vote App   │                         │  Result App  │      │
│  │  (Frontend)  │                         │ (Dashboard)  │      │
│  │ Python/Flask │                         │   Node.js    │      │
│  └──────┬───────┘                         └──────┬───────┘      │
│         │                                        │               │
│         ▼                                        ▼               │
│  ┌──────────────┐                         ┌──────────────┐      │
│  │    Redis     │◀────────────────────────│  PostgreSQL  │      │
│  │   (Cache)    │      ┌──────────────┐   │  (Database)  │      │
│  └──────────────┘      │  Worker App  │   └──────────────┘      │
│         ▲              │  (Processor) │          ▲               │
│         │              │     Java     │          │               │
│         └──────────────┴──────────────┴──────────┘               │
└─────────────────────────────────────────────────────────────────┘
```

## Components

| Component | Directory | Technology | Purpose | Dependencies |
|-----------|-----------|------------|---------|--------------|
| Vote App | [vote-app/](./vote-app/) | Python/Flask | Voting interface | Redis endpoint |
| Result App | [result-app/](./result-app/) | Node.js | Results dashboard | PostgreSQL endpoint + secret |
| Worker App | [worker-app/](./worker-app/) | Java | Vote processor | Redis + PostgreSQL endpoints |

### WebStack RGD

The [webstack-rgd/](./webstack-rgd/) directory contains a unified RGD that can deploy any of the three application types through a single interface, demonstrating the **polymorphic RGD pattern**.

## Usage

### 1. Apply Component RGDs

```bash
kubectl apply -f vote-app/rg.yaml
kubectl apply -f result-app/rg.yaml
kubectl apply -f worker-app/rg.yaml
```

### 2. Verify RGDs are Active

```bash
kubectl get rgd
```

Expected output:
```
NAME                    APIVERSION   KIND        STATE    AGE
vote-app.kro.run        v1alpha1     VoteApp     Active   1m
result-app.kro.run      v1alpha1     ResultApp   Active   1m
worker-app.kro.run      v1alpha1     WorkerApp   Active   1m
```

### 3. Create Instances

#### Standalone Deployment

Deploy each component independently for testing:

```bash
# Deploy vote app
kubectl apply -f vote-app/instance.yaml

# Deploy result app
kubectl apply -f result-app/instance.yaml

# Deploy worker app
kubectl apply -f worker-app/instance.yaml
```

#### Composed Application

Use these components in larger ResourceGraphDefinitions:

```yaml
resources:
- id: voteapp
  template:
    apiVersion: kro.run/v1alpha1
    kind: VoteApp
    spec:
      name: my-vote-app
      redisEndpoint: ${redis.status.endpoint}
      ingress:
        enabled: true
```

### 4. Verify Instances

```bash
kubectl get voteapp,resultapp,workerapp
```

## Configuration Options

### Vote App Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | string | required | Application name |
| `namespace` | string | "default" | Kubernetes namespace |
| `image` | string | - | Container image |
| `replicas` | integer | 1 | Number of replicas |
| `redisEndpoint` | string | required | Redis endpoint URL |
| `service.enabled` | boolean | true | Create Service |
| `ingress.enabled` | boolean | false | Create Ingress |

### Result App Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | string | required | Application name |
| `namespace` | string | "default" | Kubernetes namespace |
| `image` | string | - | Container image |
| `replicas` | integer | 1 | Number of replicas |
| `postgresEndpoint` | string | required | PostgreSQL endpoint |
| `postgresSecretName` | string | required | Secret with DB credentials |
| `service.enabled` | boolean | true | Create Service |
| `ingress.enabled` | boolean | false | Create Ingress |

### Worker App Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | string | required | Application name |
| `namespace` | string | "default" | Kubernetes namespace |
| `image` | string | - | Container image |
| `replicas` | integer | 1 | Number of replicas |
| `redisEndpoint` | string | required | Redis endpoint URL |
| `postgresEndpoint` | string | required | PostgreSQL endpoint |
| `postgresSecretName` | string | required | Secret with DB credentials |

## Database Dependencies

These Kubernetes components require database backends:

| Database | RGD Location | Purpose |
|----------|--------------|---------|
| PostgreSQL | [../../aws/rds-postgres/](../../aws/rds-postgres/) | Persistent vote storage |
| Redis | [../../aws/elasticache-serverless/](../../aws/elasticache-serverless/) | Vote queue/cache |

## Complete Example

See [../../aws/dogsvscats/](../../aws/dogsvscats/) for a complete composed application that combines these Kubernetes components with AWS infrastructure RGDs.

## Benefits

1. **Cloud-Agnostic**: Pure Kubernetes resources work with any PostgreSQL/Redis backend
2. **Reusable**: Components can be used independently or composed together
3. **Configurable**: Flexible deployment options via feature flags
4. **Testable**: Each component can be tested in isolation
5. **Composable**: Parent RGDs can wire these together with infrastructure

## Clean Up

Remove instances:
```bash
kubectl delete voteapp,resultapp,workerapp --all
```

Remove RGDs:
```bash
kubectl delete rgd vote-app.kro.run result-app.kro.run worker-app.kro.run
```
