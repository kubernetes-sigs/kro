# AWS Accounts Factory

An end-to-end AWS account provisioning pipeline that creates new AWS accounts, sets up networking, and deploys EKS clusters, all from a single Kubernetes custom resource.

## Description

This example demonstrates kro's **nested ResourceGraphDefinition** capability by composing three layered RGDs into a full account infrastructure factory:

1. **`NetworkStack`** (`01-network-stack.yaml`) — Creates a complete VPC with public/private subnets, internet gateway, NAT gateway, route tables, and security groups using ACK EC2 controllers.

2. **`EKSClusterStack`** (`02-eks-cluster-stack.yaml`) — Provisions an EKS cluster with IAM roles, a managed node group, and logging configuration using ACK EKS and IAM controllers.

3. **`FullAccountInfrastructure`** (`03-full-account-infrastructure.yaml`) — The top-level orchestrator that creates an AWS Organizations account, configures cross-account IAM role selection, then composes the `NetworkStack` and `EKSClusterStack` RGDs to deliver a fully provisioned account.

## Prerequisites

- EKS cluster with kro installed
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [AWS ACK](https://aws-controllers-k8s.github.io/community/docs/community/overview/) controllers for EC2, EKS, IAM, and Organizations
- AWS Organizations enabled in the management account

## Setup

### 1. Install ACK Controllers

Install the ACK controllers for each required service. You can install them via Helm charts or use the kro-based approach from the [`ack-controller`](../ack-controller/) example.

The following ACK controllers are required:

| Controller | Used By | Purpose |
|---|---|---|
| EC2 | `NetworkStack` | VPC, subnets, internet gateway, NAT gateway, route tables, security groups |
| EKS | `EKSClusterStack` | EKS cluster and managed node group |
| IAM | `EKSClusterStack` | Cluster and node IAM roles |
| Organizations | `FullAccountInfrastructure` | AWS account creation |

See the [ACK documentation](https://aws-controllers-k8s.github.io/community/docs/user-docs/install/) for installation instructions.

### 2. Configure IAM for ACK Controllers

Each ACK controller needs an IAM role to interact with AWS APIs. The IAM role for each controller must have permissions for its respective AWS service (e.g., the EC2 controller role needs EC2 permissions, the EKS controller role needs EKS permissions, etc.).

The EKS cluster can run in a **delegated account**. The Organizations `CreateAccount` API can only be called from the management account, so the setup differs depending on where your cluster lives.

#### Cluster in a delegated account

If the EKS cluster runs in a separate account (common in organizations that lock down the management account), the Organizations ACK controller needs to **assume a role in the management account** to call `CreateAccount`. Create a role in the management account with `organizations:CreateAccount` and `organizations:DescribeAccount` permissions, and a trust policy that allows the Organizations controller role in the cluster account to assume it:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "AWS": "arn:aws:iam::<cluster-account-id>:role/ack-organizations-controller-role"
      },
      "Action": "sts:AssumeRole"
    }
  ]
}
```

Then configure the Organizations ACK controller to assume this role using the `--aws-account-id` and `--aws-role-arn` configuration options, or via the ACK [cross-account resource management](https://aws-controllers-k8s.github.io/community/docs/user-docs/cross-account-resource-management/) setup.

The other ACK controllers (EC2, EKS, IAM) do not need access to the management account — they only need `sts:AssumeRole` permission to assume the `OrganizationAccountAccessRole` in the newly created destination accounts. This is configured via the `IAMRoleSelector` in step 3.

### Credential Provider: IRSA vs EKS Pod Identity

You can use either **IRSA** or **EKS Pod Identity** to provide credentials to the ACK controllers, but each has different implications for cross-account access in this example.

#### Option A: IRSA 

Set up [IRSA (IAM Roles for Service Accounts)](https://aws-controllers-k8s.github.io/community/docs/user-docs/irsa/) for each ACK controller. With IRSA, the controllers use `AssumeRoleWithWebIdentity` for their base credentials, then perform a plain `sts:AssumeRole` for cross-account access via the `IAMRoleSelector`. This works with the default `OrganizationAccountAccessRole` trust policy out of the box, since it only requires `sts:AssumeRole`.

#### Option B: EKS Pod Identity

EKS Pod Identity can be used, but requires an additional step. By default, Pod Identity uses `sts:TagSession` to attach session tags (cluster name, namespace, service account, etc.) when assuming roles. The `OrganizationAccountAccessRole` that AWS Organizations automatically creates in new accounts only allows `sts:AssumeRole` in its trust policy — not `sts:TagSession` — so cross-account assumption will fail with the default configuration.

To work around this, disable session tags when creating the Pod Identity association:

```bash
aws eks create-pod-identity-association \
  --cluster-name <cluster-name> \
  --namespace ack-system \
  --service-account <ack-controller-sa> \
  --role-arn arn:aws:iam::<cluster-account-id>:role/ack-controller-role \
  --disable-session-tags
```

The role needs `sts:AssumeRole` permission to the `OrganizationAccountAccessRole` in the destination accounts. You can also disable session tags on existing associations via the EKS console: navigate to your cluster's **Access** tab, select the Pod Identity association, choose **Edit**, and select **Disable session tags**.

> **Note:** The Pod Identity role must reside in the same AWS account as the EKS cluster due to IAM `PassRole` requirements.

> **Note:** Disabling session tags removes the ability to use [ABAC (Attribute-Based Access Control)](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-abac.html) policies based on Pod Identity session tags for these controllers.

### 3. Configure Cross-Account Access with IAMRoleSelector

The `FullAccountInfrastructure` RGD uses an ACK `IAMRoleSelector` to enable cross-account resource creation. When a new AWS account is created via Organizations, ACK needs to assume a role in that account to create VPC, EKS, and IAM resources.

This works as follows:

1. AWS Organizations creates the new account with a default role (e.g., `OrganizationAccountAccessRole`)
2. The `IAMRoleSelector` resource tells ACK to assume `arn:aws:iam::<new-account-id>:role/OrganizationAccountAccessRole` for resources in the account's namespace
3. ACK controllers then create VPC, EKS, and IAM resources in the new account using that role

For this to work, your ACK controller IAM roles (in the cluster account) must have permission to assume the `OrganizationAccountAccessRole` in the target accounts. Add the following to each ACK controller's IAM role permissions policy:

```json
{
  "Effect": "Allow",
  "Action": "sts:AssumeRole",
  "Resource": "arn:aws:iam::*:role/OrganizationAccountAccessRole"
}
```

> **Note:** The `OrganizationAccountAccessRole` is automatically created by AWS Organizations in new accounts. You can customize the role name via the `accountRoleName` parameter.

For more details, see the [ACK Cross-Account Resource Management documentation](https://aws-controllers-k8s.github.io/community/docs/user-docs/cross-account-resource-management/).

## Usage

### Deploy the ResourceGraphDefinitions

Apply the RGDs in order, since the top-level RGD depends on the others:

```bash
kubectl apply -f 01-network-stack.yaml
kubectl apply -f 02-eks-cluster-stack.yaml
kubectl apply -f 03-full-account-infrastructure.yaml
```

Verify the ResourceGraphDefinitions:

```bash
kubectl get ResourceGraphDefinition
```

Expected output:

```
NAME                          AGE
network-stack                 1m
eks-cluster-stack             1m
full-account-infrastructure   1m
```

### Create a new account with full infrastructure

Review and customize the instance file:

```bash
cat demo-account.yaml
```

Apply the instance:

```bash
kubectl apply -f demo-account.yaml
```

### Check status

```bash
kubectl get fullaccountinfrastructure acme-demo
```

### Get details

```bash
echo "Account ID: $(kubectl get fullaccountinfrastructure acme-demo -o jsonpath='{.status.accountId}')"
echo "VPC ID: $(kubectl get fullaccountinfrastructure acme-demo -o jsonpath='{.status.vpcId}')"
echo "Cluster Endpoint: $(kubectl get fullaccountinfrastructure acme-demo -o jsonpath='{.status.clusterEndpoint}')"
```

## Configuration

The `FullAccountInfrastructure` custom resource accepts the following parameters:

| Parameter | Type | Default | Description |
|---|---|---|---|
| `accountName` | string | *required* | Name for the new AWS account |
| `accountEmail` | string | *required* | Email for the new AWS account |
| `accountRoleName` | string | `OrganizationAccountAccessRole` | IAM role name for cross-account access |
| `region` | string | *required* | AWS region for resources |
| `vpcCidr` | string | *required* | CIDR block for the VPC |
| `availabilityZones` | []string | *required* | List of availability zones |
| `clusterVersion` | string | *required* | EKS Kubernetes version |
| `nodeInstanceType` | string | *required* | EC2 instance type for worker nodes |
| `nodeGroupDesiredSize` | integer | `1` | Desired number of worker nodes |
| `nodeGroupMinSize` | integer | `0` | Minimum number of worker nodes |
| `nodeGroupMaxSize` | integer | `2` | Maximum number of worker nodes |

## Clean up

Delete the instance first, then the RGDs:

```bash
kubectl delete fullaccountinfrastructure acme-demo
kubectl delete -f 03-full-account-infrastructure.yaml
kubectl delete -f 02-eks-cluster-stack.yaml
kubectl delete -f 01-network-stack.yaml
```

## Notes

- This example showcases kro's ability to nest RGDs, allowing complex infrastructure to be composed from reusable building blocks.
- The `NetworkStack` and `EKSClusterStack` RGDs can also be used independently for more granular control.
- Cross-account resource creation is handled via the ACK `IAMRoleSelector`, which configures ACK to assume the Organizations role in the new account.
- The `forEach` construct in the `NetworkStack` RGD dynamically creates subnets based on the number of availability zones provided.

