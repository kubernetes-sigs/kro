# Result App Component

A standalone result dashboard component for the Dogs vs Cats voting system.

## Description

This component creates a Node.js application that displays real-time voting results from PostgreSQL database.

## Prerequisites

- Kubernetes cluster
- PostgreSQL endpoint available
- PostgreSQL secret with password
- AWS Load Balancer Controller (if using ingress)

## Usage

### Apply the ResourceGraphDefinition

```bash
kubectl apply -f rg.yaml
```

### Create an instance

Update the `postgresEndpoint` and `postgresSecretName` in `instance.yaml`, then:

```bash
kubectl apply -f instance.yaml
```

### Check status

```bash
kubectl get resultapp result-app-demo
```

### Get the URL (if ingress enabled)

```bash
echo "Result App: http://$(kubectl get resultapp result-app-demo -o jsonpath='{.status.url}')"
```

## Configuration

- `image`: Container image for the result app (default: `dogsvscats/result:latest`)
- `replicas`: Number of replicas (default: 1)
- `postgresEndpoint`: PostgreSQL server endpoint (required)
- `postgresSecretName`: Secret containing PostgreSQL password (required)
- `service.enabled`: Create ClusterIP service (default: true)
- `ingress.enabled`: Create ALB ingress (default: false)

## Clean up

```bash
kubectl delete resultapp result-app-demo
kubectl delete rgd result-app.kro.run
```