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
        ├── vote-instance.yaml   # Vote frontend (creates ElastiCache)
        ├── result-instance.yaml # Result dashboard (creates RDS)
        └── worker-instance.yaml # Background processor (external refs)
```

## Prerequisites

- ACK controllers installed for RDS and ElastiCache
- The dependent RGDs applied:
  - `../../aws/rds-postgres/rg.yaml` → creates `RDSPostgres` kind
  - `../../aws/elasticache-serverless/rg.yaml` → creates `ElastiCacheServerless` kind

## Quick Start

```bash
# Apply dependent AWS RGDs first
kubectl apply -f ../../aws/rds-postgres/rg.yaml
kubectl apply -f ../../aws/elasticache-serverless/rg.yaml

# Apply the WebStack RGD
kubectl apply -f webstack-rgd/web-stack-rgd.yaml

# Verify RGDs are active
kubectl get rgd

# Update instance files with your VPC ID and subnet IDs, then deploy
kubectl apply -f webstack-rgd/instances/

# Verify instances
kubectl get webstack
```

## Instance Examples

| Instance | Description | Infrastructure |
|----------|-------------|----------------|
| [vote-instance.yaml](./webstack-rgd/instances/vote-instance.yaml) | Vote frontend with Ingress | Creates ElastiCacheServerless |
| [result-instance.yaml](./webstack-rgd/instances/result-instance.yaml) | Result dashboard with Ingress | Creates RDSPostgres |
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
| `elasticache.enabled` | boolean | `false` | Create managed ElastiCacheServerless |
| `rds.enabled` | boolean | `false` | Create managed RDSPostgres |
| `aws.vpcID` | string | `""` | VPC ID (required when elasticache/rds enabled) |
| `aws.subnetIDs` | []string | `[]` | Subnet IDs (required when elasticache/rds enabled) |
| `externalElasticache.enabled` | boolean | `false` | Reference existing ElastiCacheServerless |
| `externalElasticache.name` | string | `""` | Name of existing ElastiCacheServerless |
| `externalElasticache.namespace` | string | `""` | Namespace of existing ElastiCacheServerless |
| `externalRds.enabled` | boolean | `false` | Reference existing RDSPostgres |
| `externalRds.name` | string | `""` | Name of existing RDSPostgres |
| `externalRds.namespace` | string | `""` | Namespace of existing RDSPostgres |
| `externalPostgresSecret.enabled` | boolean | `false` | Reference existing secret |
| `externalPostgresSecret.name` | string | `""` | Name of existing secret |
| `externalPostgresSecret.namespace` | string | `""` | Namespace of existing secret |

## Clean Up

```bash
kubectl delete webstack --all
kubectl delete rgd webstack.kro.run
```
