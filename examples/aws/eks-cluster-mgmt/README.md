# Amazon EKS cluster management using kro & ACK

This example demonstrates how to manage a fleet of EKS clusters using kro, ACK,
and ArgoCD -- it creates EKS clusters, and bootstraps them with the required
add-ons

A hub-spoke model is used in this example; a management cluster (hub) is created
as part of the initial setup and the controllers needed for provisioning and
bootstrapping workload clusters (spokes) are installed on top.

![EKS cluster management using kro & ACK](docs/eks-cluster-mgmt-central.drawio.png)

**NOTE:** As this example evolves, some of the instructions below will be
detailed further (e.g. the creation of the management cluster), others (e.g.
controllers installation) will be automated via the GitOps flow.

## Prerequisites

1. AWS account for the management cluster
2. AWS account for workload clusters; each with the following IAM roles:

   - `eks-cluster-mgmt-ec2`
   - `eks-cluster-mgmt-eks`
   - `eks-cluster-mgmt-iam`

   The permissions should be as needed for every controller. Trust policy:

   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": {
           "AWS": "arn:aws:iam::<mgmt-account-id>:role/ack-<srvc-name>-controller"
         },
         "Action": "sts:AssumeRole",
         "Condition": {}
       }
     ]
   }
   ```

## Instructions

### Environment variables

1. Use the snippet below to set environment variables. Replace the placeholders
   first (surrounded with`<>`):

```sh
export KRO_REPO_URL="https://github.com/kro-run/kro.git"
export WORKSPACE_PATH=<workspace-path> #the directory where repos will be cloned e.g. ~/environment
export ACCOUNT_ID=$(aws sts get-caller-identity --output text --query Account)
export AWS_REGION=<region> #e.g. us-west-2
export CLUSTER_NAME=mgmt
export ARGOCD_CHART_VERSION=7.5.2
```

### Repo

2. Clone kro repo:

```sh
git clone $KRO_REPO_URL $WORKSPACE_PATH/kro
```
3. Create the GitHub repo `gitops-fleet-management` in your organization; it will contain
   the clusters definition, the add-ons and it will be reconciled to the management cluster
   via the GitOps flow
4. Populate the repo:

```sh
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/addons/* $WORKSPACE_PATH/gitops-fleet-management/addons
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/apps/* $WORKSPACE_PATH/gitops-fleet-management/apps
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/charts/* $WORKSPACE_PATH/gitops-fleet-management/charts
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/fleet/* $WORKSPACE_PATH/gitops-fleet-management/fleet
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/platform/* $WORKSPACE_PATH/gitops-fleet-management/platform
cp -r $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/scripts/* $WORKSPACE_PATH/gitops-fleet-management/scripts

```

5. Push the changes

```sh
cd $WORKSPACE_PATH/gitops-fleet-management
git add .
git commit -m "initial setup"
git push

```

### Management cluster

6. Create an EKS cluster (management cluster)

Update $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/terraform/hub/config.auto.tfvars as follows
replace git_org_name with your own org
repalce account_ids with your fleet account ids
Apply terraform as follows
```sh
cd $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/terraform/hub
terraform apply --auto-approve
```

### Adding workload clusters

7. Update gitops-fleet-management\fleet\kro-values\tenants
    `xxx/kro-clusters/values.yaml`.
8. Commit/push the changes to Git, then wait for the sync operation to complete by checking ArgoCD UI.


## Clean-up

1. Apply terraform as follows

```sh
cd $WORKSPACE_PATH/kro/examples/eks-cluster-mgmt/terraform/hub
terraform destroy --auto-approve
```