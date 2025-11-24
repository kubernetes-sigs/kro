# RDS PostgreSQL

A reusable PostgreSQL RDS instance component.

## Description

This component creates an AWS RDS PostgreSQL instance with security group and subnet group configuration.

## Prerequisites

- EKS cluster
- AWS ACK controllers for EC2 and RDS
- VPC with private subnets

## Usage

### Apply the ResourceGraphDefinition

```bash
kubectl apply -f rg.yaml
```

### Create an instance

Set environment variables:

```bash
export POSTGRES_NAME="my-postgres-db"
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
kubectl get rdspostgres my-postgres-db
```

### Get connection details

```bash
echo "Endpoint: $(kubectl get rdspostgres my-postgres-db -o jsonpath='{.status.endpoint}')"
echo "Secret: $(kubectl get rdspostgres my-postgres-db -o jsonpath='{.status.secretName}')"
```

## Configuration

- `instanceClass`: RDS instance class (default: "db.t4g.micro")
- `allocatedStorage`: Storage in GB (default: 20)
- `engineVersion`: PostgreSQL version (default: "16.3")
- `dbName`: Database name (default: "postgres")
- `masterUsername`: Master username (default: "postgres")

## Clean up

```bash
kubectl delete rdspostgres my-postgres-db
kubectl delete rgd rds-postgres.kro.run
```

## Notes

- Uses default password "postgres" (base64 encoded)
- Security group allows access from 10.0.0.0/16 CIDR
- Backup retention is disabled for demo purposes