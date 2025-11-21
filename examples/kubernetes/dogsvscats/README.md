# Dogs vs Cats App - Kubernetes Components

Kubernetes application components for the Dogs vs Cats voting system.

## Components

### [Vote App](./vote-app/)
- **Type**: Frontend application
- **Technology**: Python/Flask
- **Purpose**: Voting interface for users
- **Dependencies**: Redis endpoint
- **Features**: Deployment, Service, optional Ingress

### [Result App](./result-app/)
- **Type**: Dashboard application  
- **Technology**: Node.js
- **Purpose**: Display voting results
- **Dependencies**: PostgreSQL endpoint and secret
- **Features**: Deployment, Service, optional Ingress

### [Worker App](./worker-app/)
- **Type**: Background processor
- **Technology**: Java
- **Purpose**: Process votes from Redis to PostgreSQL
- **Dependencies**: Redis and PostgreSQL endpoints
- **Features**: Deployment only (no service needed)

## Architecture

```
┌─────────────┐    ┌──────────────┐    ┌─────────────┐
│  Vote App   │───▶│   Database   │◀───│ Result App  │
│ (Frontend)  │    │ (PostgreSQL  │    │ (Dashboard) │
└─────────────┘    │  + Redis)    │    └─────────────┘
                   └──────────────┘           ▲
                          ▲                  │
                          │                  │
                   ┌─────────────┐           │
                   │ Worker App  │───────────┘
                   │(Processor)  │
                   └─────────────┘
```

## Usage Patterns

### Standalone Deployment
Each component can be deployed independently for testing:

```bash
# Deploy vote app with external Redis
kubectl apply -f vote-app/rg.yaml
kubectl apply -f vote-app/instance.yaml
```

### Composed Application
Use these components in larger ResourceGraphDefinitions:

```yaml
resources:
- id: voteapp
  template:
    apiVersion: kro.run/v1alpha1
    kind: VoteApp
    spec:
      redisEndpoint: ${redis.status.endpoint}
      ingress:
        enabled: true
```

## Database Dependencies

These Kubernetes components require database backends:

- **PostgreSQL**: Use [../../aws/rds-postgres/](../../aws/rds-postgres/)
- **Redis**: Use [../../aws/elasticache-serverless/](../../aws/elasticache-serverless/)

## Complete Example

See [../../aws/dogsvscats/](../../aws/dogsvscats/) for a complete composed application using these components.

## Benefits

- **Cloud-agnostic**: Pure Kubernetes resources
- **Reusable**: Work with any PostgreSQL/Redis backend
- **Configurable**: Flexible deployment options
- **Testable**: Can be tested independently