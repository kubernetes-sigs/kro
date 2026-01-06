# Dogs vs Cats - Kubernetes Application Components

Kubernetes application components for the Dogs vs Cats voting system using a unified WebStack RGD that deploys any application type through feature flags and external references.

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

## Directory Structure

```
dogsvscats/
└── webstack-rgd/
    ├── web-stack-rgd.yaml      # Unified WebStack RGD
    └── instances/
        ├── vote-instance.yaml   # Vote frontend
        ├── result-instance.yaml # Result dashboard
        └── worker-instance.yaml # Background processor
```

## Quick Start

```bash
# Apply the WebStack RGD
kubectl apply -f webstack-rgd/web-stack-rgd.yaml

# Verify RGD is active
kubectl get rgd webstack.kro.run

# Deploy all application instances
kubectl apply -f webstack-rgd/instances/

# Verify instances
kubectl get webstack
```

## Instance Examples

| Instance | Description | Infrastructure |
|----------|-------------|----------------|
| [vote-instance.yaml](./webstack-rgd/instances/vote-instance.yaml) | Vote frontend with Ingress | Creates ElastiCache |
| [result-instance.yaml](./webstack-rgd/instances/result-instance.yaml) | Result dashboard with Ingress | Creates RDS |
| [worker-instance.yaml](./webstack-rgd/instances/worker-instance.yaml) | Background processor (no service) | References external ElastiCache/RDS |

## WebStack Configuration

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | string | - | Application name |
| `namespace` | string | `default` | Kubernetes namespace |
| `image` | string | `dogsvscats/result:latest` | Container image |
| `replicas` | integer | `1` | Number of replicas |
| `service.enabled` | boolean | `true` | Create Service |
| `ingress.enabled` | boolean | `false` | Create Ingress |
| `elasticache.enabled` | boolean | `false` | Create managed ElastiCache |
| `rds.enabled` | boolean | `false` | Create managed RDS |
| `externalElasticache.enabled` | boolean | `false` | Reference existing ElastiCache |
| `externalElasticache.name` | string | `""` | Name of existing ElastiCache |
| `externalElasticache.namespace` | string | `""` | Namespace of existing ElastiCache |
| `externalRds.enabled` | boolean | `false` | Reference existing RDS |
| `externalRds.name` | string | `""` | Name of existing RDS |
| `externalRds.namespace` | string | `""` | Namespace of existing RDS |
| `externalPostgresSecret.enabled` | boolean | `false` | Reference existing secret |
| `externalPostgresSecret.name` | string | `""` | Name of existing secret |
| `externalPostgresSecret.namespace` | string | `""` | Namespace of existing secret |

## Clean Up

```bash
kubectl delete webstack --all
kubectl delete rgd webstack.kro.run
```
