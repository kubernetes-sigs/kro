# Dogs vs Cats Voting App

A multi-tier voting application demonstrating Kro's resource composition capabilities with AWS services.

> **Note**: This example composes multiple reusable components. Each component can also be deployed independently. See the [Component Dependencies](#component-dependencies) section below.

## Architecture

This example showcases a complete voting application composed of reusable components:

- **Vote App**: Frontend for casting votes (Python/Flask)
- **Result App**: Results dashboard (Node.js)  
- **Worker App**: Processes votes from Redis to PostgreSQL (Java)
- **RDS PostgreSQL**: Stores voting results
- **ElastiCache Serverless**: Caches votes
- **Ingress**: AWS Load Balancer Controller with ALB

## Component Dependencies

This example uses the following reusable components:

- [../../kubernetes/dogsvscats/vote-app/](../../kubernetes/dogsvscats/vote-app/) - Vote frontend
- [../../kubernetes/dogsvscats/result-app/](../../kubernetes/dogsvscats/result-app/) - Results dashboard
- [../../kubernetes/dogsvscats/worker-app/](../../kubernetes/dogsvscats/worker-app/) - Vote processor
- [../rds-postgres/](../rds-postgres/) - PostgreSQL database
- [../elasticache-serverless/](../elasticache-serverless/) - Redis cache

## Prerequisites

- EKS cluster with AWS Load Balancer Controller installed
- Kro installed and running
- AWS ACK controllers for EC2, RDS, and ElastiCache
- VPC with private subnets

## Create ResourceGraphDefinitions

Change directory to `examples`:

```
cd examples/
```

Apply the component RGs first:

```bash
# Apply Kubernetes app components
kubectl apply -f kubernetes/dogsvscats/vote-app/rg.yaml
kubectl apply -f kubernetes/dogsvscats/result-app/rg.yaml
kubectl apply -f kubernetes/dogsvscats/worker-app/rg.yaml

# Apply AWS infrastructure components
kubectl apply -f aws/rds-postgres/rg.yaml
kubectl apply -f aws/elasticache-serverless/rg.yaml
```

Apply the main composed RG:

```bash
kubectl apply -f aws/dogsvscats-appstack/dogsvscats-rg.yaml
```

Validate the RGs statuses are Active:

```
kubectl get rgd
```

Expected result:

```
NAME                              APIVERSION   KIND                   STATE    AGE
dogsvscats-app.kro.run            v1alpha1     DogsvsCatsApp          Active   1m
elasticache-serverless.kro.run    v1alpha1     ElastiCacheServerless  Active   2m
rds-postgres.kro.run              v1alpha1     RDSPostgres            Active   2m
result-app.kro.run                v1alpha1     ResultApp              Active   2m
vote-app.kro.run                  v1alpha1     VoteApp                Active   2m
worker-app.kro.run                v1alpha1     WorkerApp              Active   2m
```

## Create an Instance of kind DogsvsCatsApp

Create environment variables for your infrastructure:

```
export WORKLOAD_NAME="dogsvscats-voting-app"
export VPC_ID="vpc-xxxxxxxxx"
export SUBNET_IDS='"subnet-xxxxxxxxx", "subnet-yyyyyyyyy", "subnet-zzzzzzzzz"'
```

Validate the variables are populated:

```
echo $WORKLOAD_NAME
echo $VPC_ID
echo $SUBNET_IDS
```

Run the following command to replace the variables in `instance-template.yaml` and create `instance.yaml`:

```shell
envsubst < "dogsvscats-appstack/instance-template.yaml" > "dogsvscats-appstack/instance.yaml"
```

Apply the `dogsvscats-appstack/instance.yaml`:

```
kubectl apply -f dogsvscats-appstack/instance.yaml
```

Validate instance status:

```
kubectl get dogsvscatsapp dogsvscats-voting-app
```

Expected result:

```
NAME                    STATE    SYNCED   AGE
dogsvscats-voting-app   ACTIVE   True     5m
```

## Validate the app is working

Get the URLs:

```
echo "Vote App: http://$(kubectl get dogsvscatsapp dogsvscats-voting-app -o jsonpath='{.status.voteURL}')"
echo "Result App: http://$(kubectl get dogsvscatsapp dogsvscats-voting-app -o jsonpath='{.status.resultURL}')"
```

Navigate to the vote URL to cast votes, then check the result URL to see the results.

## Clean votes for demo

Reset votes between demos:

```bash
./dogsvscats/scripts/clean-votes.sh
```

## What This Example Demonstrates

1. **Component Composition**: Building complex applications from reusable components
2. **Cross-namespace References**: Kubernetes components referencing AWS infrastructure
3. **Status Propagation**: Passing endpoints and connection details between components
4. **Conditional Resources**: Ingress resources created based on configuration
5. **Multi-cloud Patterns**: Separating cloud-agnostic apps from cloud-specific infrastructure

## Clean up

Remove the instance:

```
kubectl delete dogsvscatsapp dogsvscats-voting-app
```

Remove the ResourceGraphDefinitions:

```bash
# Remove main composed RG
kubectl delete rgd dogsvscats-app.kro.run

# Remove component RGs
kubectl delete rgd vote-app.kro.run result-app.kro.run worker-app.kro.run
kubectl delete rgd rds-postgres.kro.run elasticache-serverless.kro.run
```