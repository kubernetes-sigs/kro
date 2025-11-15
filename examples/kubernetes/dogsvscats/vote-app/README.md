# Vote App Component

A standalone vote application component for the Dogs vs Cats voting system.

## Description

This component creates a Python/Flask frontend application that allows users to cast votes between Dogs and Cats. Votes are stored in Redis.

## Prerequisites

- Kubernetes cluster
- Redis endpoint available
- AWS Load Balancer Controller (if using ingress)

## Usage

### Apply the ResourceGraphDefinition

```bash
kubectl apply -f rg.yaml
```

### Create an instance

Update the `redisEndpoint` in `instance.yaml` with your Redis endpoint, then:

```bash
kubectl apply -f instance.yaml
```

### Check status

```bash
kubectl get voteapp vote-app-demo
```

### Get the URL (if ingress enabled)

```bash
echo "Vote App: http://$(kubectl get voteapp vote-app-demo -o jsonpath='{.status.url}')"
```

## Configuration

- `image`: Container image for the vote app (default: `dogsvscats/vote:latest`)
- `replicas`: Number of replicas (default: 2)
- `redisEndpoint`: Redis server endpoint (required)
- `service.enabled`: Create ClusterIP service (default: true)
- `ingress.enabled`: Create ALB ingress (default: false)

## Clean up

```bash
kubectl delete voteapp vote-app-demo
kubectl delete rgd vote-app.kro.run
```