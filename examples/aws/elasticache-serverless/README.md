# ElastiCache Serverless

A reusable Redis ElastiCache Serverless component.

## Description

This component creates an AWS ElastiCache Serverless Redis cache with security group configuration.

## Prerequisites

- EKS cluster
- AWS ACK controllers for EC2 and ElastiCache
- VPC with private subnets

## Usage

### Apply the ResourceGraphDefinition

```bash
kubectl apply -f rg.yaml
```

### Create an instance

Set environment variables:

```bash
export REDIS_NAME="my-redis-cache"
export VPC_ID="vpc-xxxxxxxxx"
export SUBNET_IDS='"subnet-xxxxxxxxx", "subnet-yyyyyyyyy", "subnet-zzzzzzzzz"'
```

Create the instance:

```bash
envsubst < "instance-template.yaml" > "instance.yaml"
kubectl apply -f instance.yaml
```

### Check status

```bash
kubectl get elasticacheserverless my-redis-cache
```

### Get connection details

```bash
echo "Endpoint: $(kubectl get elasticacheserverless my-redis-cache -o jsonpath='{.status.endpoint}')"
echo "Status: $(kubectl get elasticacheserverless my-redis-cache -o jsonpath='{.status.status}')"
```

## Configuration

- `engine`: Cache engine (default: "redis")
- `dataStorageLimit`: Data storage limit in GB (default: 1)
- `ecpuLimit`: eCPU per second limit (default: 1000)
- `description`: Cache description

## Clean up

```bash
kubectl delete elasticacheserverless my-redis-cache
kubectl delete rgd elasticache-serverless.kro.run
```

## Notes

- Uses TLS encryption by default for serverless cache
- Security group allows access from 10.0.0.0/16 CIDR
- Serverless caches automatically scale based on usage