# Worker App Component

A standalone worker component for the Dogs vs Cats voting system.

## Description

This component creates a Java application that processes votes from Redis and stores them in PostgreSQL database.

## Prerequisites

- Kubernetes cluster
- Redis endpoint available
- PostgreSQL endpoint available
- PostgreSQL secret with password

## Usage

### Apply the ResourceGraphDefinition

```bash
kubectl apply -f rg.yaml
```

### Create an instance

Update the `redisEndpoint`, `postgresEndpoint`, and `postgresSecretName` in `instance.yaml`, then:

```bash
kubectl apply -f instance.yaml
```

### Check status

```bash
kubectl get workerapp worker-app-demo
```

## Configuration

- `image`: Container image for the worker app (default: `dogsvscats/worker:latest`)
- `replicas`: Number of replicas (default: 1)
- `redisEndpoint`: Redis server endpoint (required)
- `postgresEndpoint`: PostgreSQL server endpoint (required)
- `postgresSecretName`: Secret containing PostgreSQL password (required)

## Clean up

```bash
kubectl delete workerapp worker-app-demo
kubectl delete rgd worker-app.kro.run
```