# Amazon EKS cluster management using kro & ACK

This example demonstrates how to manage a fleet of Amazon EKS clusters using kro, ACK (AWS Controllers for Kubernetes), and Argo CD across multiple regions and accounts. You'll learn how to create EKS clusters and bootstrap them with required add-ons.

The solution implements a hub-spoke model where a management cluster (hub) is created during initial setup, with EKS capabilities (kro, ACK and Argo CD) enabled for provisioning and bootstrapping workload clusters (spokes) via a GitOps flow.

![EKS cluster management using kro & ACK](docs/eks-cluster-mgmt-central.drawio.png)

## Prerequisites

1. AWS account for the management cluster, and optional AWS accounts for spoke clusters (management account can be reused for spokes)
2. AWS IAM Identity Center (IdC) is enabled in the management account 
3. GitHub account and a valid GitHub Token
4. GitHub [cli](https://cli.github.com/)
5. Argo CD [cli](https://argo-cd.readthedocs.io/en/stable/cli_installation/)
6. Terraform [cli](https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli)
7. AWS [cli v2.32.27+](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html)

## Instructions

### Configure workspace

1. Create variables

   First, set these environment variables that typically don't need modification:

   ```sh
   export KRO_REPO_URL="https://github.com/kubernetes-sigs/kro.git"
   export WORKING_REPO="eks-cluster-mgmt" # Try to keep this default name as it is referenced in terraform and gitops configs
   export TF_VAR_FILE="terraform.tfvars" # the name of terraform configuration file to use
   ```

   Then customize these variables for your specific environment:

   ```sh
   export MGMT_ACCOUNT_ID=$(aws sts get-caller-identity --output text --query Account) # Or update to the AWS account to use for your management cluster
   export AWS_REGION="eu-west-2" # change to your preferred region
   export WORKSPACE_PATH="$HOME" # the directory where repos will be cloned
   export GITHUB_ORG_NAME="iamahgoub" # your Github username or organization name
   ```

2. Clone kro repository

   ```sh
   git clone $KRO_REPO_URL $WORKSPACE_PATH/kro
   ```

3. Create your working GitHub repository

   Create a new repository using the GitHub CLI or through the GitHub website:

   ```sh
   gh repo create $WORKING_REPO --private
   ```

4. Clone the working empty git repository

   ```sh
   gh repo clone $WORKING_REPO $WORKSPACE_PATH/$WORKING_REPO
   ```

5. Populate the repository

   ```sh
   cp -r $WORKSPACE_PATH/kro/examples/aws/eks-cluster-mgmt/* $WORKSPACE_PATH/$WORKING_REPO/
   ```

6. Update the Spoke accounts

   If deploying EKS clusters across multiple AWS accounts, update the configuration below. Even for single account deployments, you must specify the AWS account for each namespace.

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/addons/tenants/tenant1/default/addons/multi-acct/values.yaml
   ```

   Values:

   ```yaml
   clusters:
      workload-cluster1: "012345678910" # AWS account for workload cluster 1
      workload-cluster2: "123456789101" # AWS account for workload cluster 2
   ```

   > Note: If you only want to use 1 AWS account, reuse the AWS account of your management cluster for the other workload clusters.

7. Add, Commit and Push changes

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "initial commit"
   git push
   ```

### Create the Management cluster

1. Update the terraform.tfvars with your values

   Modify the terraform.tfvars file with your GitHub working repo details:
   - Set `git_org_name`
   - Update any `gitops_xxx` values if you modified the proposed setup (git path, branch...)
   - Confirm `gitops_xxx_repo_name` is "eks-cluster-mgmt" (or update if modified)
   - Configure `accounts_ids` with the list of AWS accounts for spoke clusters (use management account ID if creating spoke clusters in the same account)

   ```sh
   # edit: terraform.tfvars
   code $WORKSPACE_PATH/$WORKING_REPO/terraform/hub/terraform.tfvars
   ```

1. Log in to your AWS management account

   Connect to your AWS management account using your preferred authentication method:

   ```sh
   export AWS_PROFILE=management_account # use your own profile or ensure you're connected to the appropriate account
   ```

1. Apply the terraform to create the management cluster:

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/terraform/hub
   ./install.sh
   ```

   Review the proposed changes and accept to deploy.

   Note: EKS capabilities are not supported yet by Terraform AWS provider. So, we will create them manually using CLI commands.

1. Retrieve terraform outputs and set into environment variables:

   ```sh
   export CLUSTER_NAME=$(terraform output -raw cluster_name)
   export ACK_CONTROLLER_ROLE_ARN=$(terraform output -raw ack_controller_role_arn)
   export KRO_CONTROLLER_ROLE_ARN=$(terraform output -raw kro_controller_role_arn)
   export ARGOCD_CONTROLLER_ROLE_ARN=$(terraform output -raw argocd_controller_role_arn)
   ```

1. Create ACK capability
   ```sh
   aws eks create-capability \
   --region ${AWS_REGION} \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-ack \
   --type ACK \
   --role-arn ${ACK_CONTROLLER_ROLE_ARN} \
   --delete-propagation-policy RETAIN
   ```

1. Now, we need to create the Argo CD capability -- IdC has to be enabled in the management account for that. So before creating the capability let's store the IdC instance details and the user that will be used for accessing Argo CD in environment variables:

   ```sh
   export IDC_INSTANCE_ARN='<replace with IdC instance ARN>'
   export IDC_USER_ID='<replace with IdC user id>'
   export IDC_REGION='<replace with IdC region>'
   ```
1. Create Argo CD capability

   ```sh
   aws eks create-capability \
   --region ${AWS_REGION} \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-argocd \
   --type ARGOCD \
   --role-arn ${ARGOCD_CONTROLLER_ROLE_ARN} \
   --delete-propagation-policy RETAIN \
   --configuration '{
      "argoCd": {
         "awsIdc": {
         "idcInstanceArn": "'${IDC_INSTANCE_ARN}'",
         "idcRegion": "'${IDC_REGION}'"
         },
         "rbacRoleMappings": [{
         "role": "ADMIN",
         "identities": [{
            "id": "'${IDC_USER_ID}'",
            "type": "SSO_USER"
         }]
         }]
      }
   }'
   ```

1. Create kro capability

   ```sh
   aws eks create-capability \
   --region ${AWS_REGION} \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-kro \
   --type KRO \
   --role-arn ${KRO_CONTROLLER_ROLE_ARN} \
   --delete-propagation-policy RETAIN
   ```

1. Make sure all the capabilities are now enabled by checking status using the console or the `describe-capability` command. For example:
   ```sh
   aws eks describe-capability \
   --region ${AWS_REGION} \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-argocd \
   --query 'capability.status' \
   --output text
   ```

   Modify/run the commands above for other capabilities to make sure they are all `ACTIVE`.

1. Retrieve the ArgoCD server URL and log on using the user provided during the capability creation:
   ```sh
   export ARGOCD_SERVER=$(aws eks describe-capability \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-argocd \
   --query 'capability.configuration.argoCd.serverUrl' \
   --output text \
   --region ${AWS_REGION})

   echo ${ARGOCD_SERVER}
   ```

1. Generate an account token from the Argo CD UI (Settings → Accounts → admin → Generate New Token), then set it as an environment variable:
   ```sh
   export ARGOCD_AUTH_TOKEN="<your-token-here>"
   export ARGOCD_OPTS="--grpc-web"
   ```

1. Configure GitHub repository access (if using private repository). We automate this process using the Argo CD CLI. You can also configure this in the Web interface under "Settings / Repositories"

   ```sh
   export GITHUB_TOKEN="<your-token-here>"
   argocd repo add https://github.com/$GITHUB_ORG_NAME/$WORKING_REPO.git --username iamahgoub --password $GITHUB_TOKEN --upsert --name github
   ```

   Note: If you encounter the error "Failed to load target state: failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = authentication required", verify your GitHub token settings.

1. Connect to the cluster

   ```sh
   aws eks update-kubeconfig --name hub-cluster
   ```

1. Install Argo CD App of App:
   ```sh
   kubectl apply -f $WORKSPACE_PATH/$WORKING_REPO/terraform/hub/bootstrap/applicationsets.yaml
   ``` 

### Bootstrap Spoke accounts

For the management cluster to create resources in the spoke AWS accounts, we need to create an IAM roles in the spoke accounts to be assumed by the ACK capability in the management account for that purpose.

> Note: Even if you're only testing this in the management account, you still need to perform this procedure, replacing the list of spoke account numbers with the management account number.

We provide a script to help with that. You need to first connect to each of your spoke accounts and execute the script.

1. Log in to your AWS Spoke account

   Connect to your AWS spoke account. This example uses specific profiles, but adapt this to your own setup:

   ```sh
   export AWS_PROFILE=spoke_account1 # use your own profile or ensure you're connected to the appropriate account
   ```

2. Execute the script to configure IAM roles

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/scripts
   ./create_ack_workload_roles.sh
   ```

> Repeat this step for each spoke account you want to use with the solution

### Create a Spoke cluster

Update $WORKSPACE_PATH/$WORKING_REPO

1. Add cluster creation by kro

   Edit the file:

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml
   ```

   Configure the AWS accounts for management and spoke accounts:

   ```yaml
      workload-cluster1:
      managementAccountId: "012345678910"    # replace with your management cluster AWS account ID
      accountId: "123456789101"              # replace with your spoke workload cluster AWS account ID (can be the same)
      tenant: "tenant1"                      # We have only configured tenant1 in the repo. If you change it, you need to duplicate all tenant1 directories
      k8sVersion: "1.30"
      workloads: "true"                      # Set to true if you want to deploy the workloads namespaces and applications
      gitops:
         addonsRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"    # replace with your github account
         fleetRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
         platformRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
         workloadRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
   ```

2. Add, Commit and Push

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "initial commit"
   git push
   ```

5. After some time, the cluster should be created in the spoke account.

   ```sh
   kubectl get EksClusterwithvpcs -A
   ```

   ```sh
   NAMESPACE   NAME                STATE    SYNCED   AGE
   argocd      workload-cluster1   ACTIVE   True     36m
   ```

   > If you see STATE=ERROR, this may be normal as it will take some time for all dependencies to be ready. Check the logs of kro and ACK controllers for possible configuration errors.

   You can also list resources created by kro to validate their status:

   ```sh
   kubectl get vpcs.kro.run -A
   kubectl get vpcs.ec2.services.k8s.aws -A -o yaml # check for errors
   ```

   > If you see errors, double-check the multi-cluster account settings and verify that IAM roles in both management and workload AWS accounts are properly configured.

   When VPCs are ready, check EKS resources:

   ```sh
   kubectl get eksclusters.kro.run -A
   kubectl get clusters.eks.services.k8s.aws -A -o yaml # Check for errors
   ```

6. Connect to the spoke cluster

   ```sh
   export AWS_PROFILE=spoke_account1 # use your own profile or ensure you're connected to the appropriate account
   ```

   Get kubectl configuration (update name and region if needed):

   ```sh
   aws eks update-kubeconfig --name workload-cluster1 --region us-west-2
   ```

   View deployed resources:

   ```sh
   kubectl get pods -A
   ```

   ```sh
   NAMESPACE          NAME                                                READY   STATUS    RESTARTS      AGE
   ack-system         efs-chart-7558bdd9d7-2n9q9                          1/1     Running   0             3m51s
   ack-system         eks-chart-7c8f7fd76c-pz49q                          1/1     Running   0             5m50s
   ack-system         iam-chart-6846dfc7bc-kqccf                          1/1     Running   0             5m50s
   external-secrets   external-secrets-cert-controller-586c6cbfd7-m5x94   1/1     Running   0             5m14s
   external-secrets   external-secrets-d699ddc68-hhgps                    1/1     Running   0             5m14s
   external-secrets   external-secrets-webhook-7f467cd6bf-ppzd5           1/1     Running   0             5m14s
   kube-system        efs-csi-controller-f7b568848-72rzc                  3/3     Running   0             4m12s
   kube-system        efs-csi-controller-f7b568848-vh85b                  3/3     Running   0             4m12s
   kube-system        efs-csi-node-5znbp                                  3/3     Running   0             4m13s
   kube-system        efs-csi-node-gzpsn                                  3/3     Running   0             4m13s
   kube-system        efs-csi-node-zbzlv                                  3/3     Running   0             4m13s
   kyverno            kyverno-admission-controller-5b4c74758b-kf2k7       1/1     Running   0             5m5s
   kyverno            kyverno-background-controller-7cf48d5b9d-f67h6      1/1     Running   0             5m5s
   kyverno            kyverno-cleanup-controller-cd4ccdd8c-4b4gp          1/1     Running   0             5m5s
   kyverno            kyverno-reports-controller-55c9f8d645-h8d57         1/1     Running   0             5m5s
   kyverno            policy-reporter-5c6c868c66-7jlxm                    1/1     Running   0             5m19s
   ```

   > This output shows that our GitOps solution has successfully deployed our addons in the cluster


You can repeat these steps for any additional clusters you want to manage.

Each cluster is created by its kro RGD, deployed to AWS using ACK controllers, and then automatically registered to Argo CD which can install addons and workloads automatically.

## Conclusion

This solution demonstrates a powerful way to manage multiple EKS clusters across different AWS accounts and regions using three key components:

1. **kro (Kubernetes Resource Orchestrator)**
   - Manages complex multi-resource deployments
   - Handles dependencies between resources
   - Provides a declarative way to define EKS clusters and their requirements

2. **AWS Controllers for Kubernetes (ACK)**
   - Enables native AWS resource management from within Kubernetes
   - Supports cross-account operations through namespace isolation
   - Manages AWS resources like VPCs, IAM roles, and EKS clusters

3. **Argo CD**
   - Implements GitOps practices for cluster configuration
   - Automatically bootstraps new clusters with required add-ons
   - Manages workload deployments across the cluster fleet

Key benefits of this architecture:

- **Scalability**: Easily add new clusters by updating Git configuration
- **Consistency**: Ensures uniform configuration across all clusters
- **Automation**: Reduces manual intervention in cluster lifecycle management
- **Separation of Concerns**: Clear distinction between infrastructure and application management
- **Audit Trail**: All changes are tracked through Git history
- **Multi-Account Support**: Secure isolation between different environments or business units

To expand this solution, you can:
- Add more clusters by replicating the configuration pattern
- Customize add-ons and workloads per cluster
- Implement different configurations for different environments (dev, staging, prod)
- Add monitoring and logging solutions across the cluster fleet
- Implement cluster upgrade strategies using the same tooling

The combination of kro, ACK, and Argo CD provides a robust, scalable, and maintainable approach to EKS cluster fleet management.

## Clean-up

Since our spoke workloads have been bootstrapped by Argo CD and kro/ACK, we need to clean them up in a specific order.

To ensure proper cleanup of workload namespaces, you can either remove Argo CD auto-sync or define exceptions using the cluster name you want to delete.

1. Deactivate workloads:

   ```sh
   code examples/aws/eks-cluster-mgmt/fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml
   ```

   ```yaml
   workload-cluster1:
      ...
      workloads: "false"
      ...
   ```

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "remove workloads from workload-cluster1"
   git push
   ```

   Sync RGD:

   ```bash
   argocd app sync clusters
   ```

   This allows Kubernetes addons to properly clean up any AWS resources like Load Balancers.

1. Deactivate addons that create resources:

   The efs-addon creates IAM roles and EKS pod identity associations in the workload environment. To prevent orphaned resources, disable these addons:

   ```yaml
   workload-cluster1:
      ...
      workloads: "false"
      ...
      addons:
         enable_ack_efs: "false"
   ```

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "remove ack efs addon"
   git push
   ```

   Sync RGD:

   ```bash
   argocd app sync clusters
   ```

1. Prune the cluster in Argo CD UI

   In the Argo CD UI, synchronize the cluster Applicationset with the prune option enabled, or use the CLI:

   ```bash
   argocd app sync clusters --prune
   ```

1. Delete the EKS capabilities on the management cluster

   ```sh
   aws eks delete-capability \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-argocd

   aws eks delete-capability \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-kro


   aws eks delete-capability \
   --cluster-name ${CLUSTER_NAME} \
   --capability-name ${CLUSTER_NAME}-ack
   ```

1. Make sure all the capabilities are deleted by checking the console or using the `describe-capability` command. For example:

1. Delete Management Cluster

   After successfully de-registering all spoke accounts, remove the workload cluster created with Terraform:

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/terraform/hub
   ./destroy.sh
   ```

1. Remove ACK IAM Roles in workload accounts

   Finally, connect to each workload account and delete the IAM roles and policies created initially:

   ```bash
   cd $WORKSPACE_PATH/$WORKING_REPO/
   ./scripts/delete_ack_workload_roles.sh ack
   ```
