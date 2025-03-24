# Amazon EKS cluster management using kro & ACK

This example demonstrates how to manage a fleet of EKS clusters using kro, ACK,
and ArgoCD across multiple regions and accounts -- it creates EKS clusters, and bootstraps them with the required
add-ons

A hub-spoke model is used in this example; a management cluster (hub) is created
as part of the initial setup and the controllers needed for provisioning and
bootstrapping workload clusters (spokes) are installed on top.

![EKS cluster management using kro & ACK](docs/eks-cluster-mgmt-central.drawio.png)

## Prerequisites

1. AWS account for the management cluster, and optional AWS accounts for spoke clusters. (you can reuse management account for spoke also)
2. GitHub account and a valid Github Token
3. GitHub [cli](https://cli.github.com/)
4. ArgoCD [cli](https://argo-cd.readthedocs.io/en/stable/cli_installation/)
5. terraform [cli](https://developer.hashicorp.com/terraform/tutorials/aws-get-started/install-cli)


## Instructions

### Configure workspace

1. Create variables

   Use thoses variables that should not need some changes

   ```sh
   export ACCOUNT_ID=$(aws sts get-caller-identity --output text --query Account) # Or update to the AWS account to use for your management cluster
   export KRO_REPO_URL="https://github.com/kro-run/kro.git"
   export WORKING_REPO="eks-cluster-mgmt" # If you can avoid changing this, as you'll need to update in both terraform and gitops configurations
   export TF_VAR_FILE="terraform.tfvars" # the name of terraform configuration file to use
   ```

   Use thoses variables but adjust them for you

   ```sh
   export MGMT_ACCOUNT_ID="012345678910" # specify your management AWS account ID
   export AWS_REGION="eu-west-2" # change to your prefered region
   export WORKSPACE_PATH="$HOME/environment" # the directory where repos will be cloned e.g. ~/environment
   export GITHUB_ORG_NAME="xxxxx" # your Github User-name or Organisation you want to use for the work
   ```

2. Clone kro repository

   ```sh
   git clone $KRO_REPO_URL $WORKSPACE_PATH/kro
   ```

3. Create your working github repository

   You can either work from the kro repo, or create a smaller repo just for this example

   You can create it with github cli gh, or in the GitHub website.

   ```sh
   gh repo create $WORKING_REPO --private
   ```

4. clone the working empty git repository

   ```sh
   gh repo clone $WORKING_REPO $WORKSPACE_PATH/$WORKING_REPO
   ````

5. populate the repository

   ```sh
   cp -r $WORKSPACE_PATH/kro/examples/aws/eks-cluster-mgmt/* $WORKSPACE_PATH/$WORKING_REPO/
   ```

6. Configure Spoke accounts in gitops for ACK controller

   ACK controllers can work cross AWS accounts, but that need to isolate resources into specific namespaces. We are doing this with this solution, where each ACK ressources will be isolated into specific namespaces.

   If you want to do deploy your eks clusters in multiple AWS accounts, you need to update this configuration.
   If you just want to only use 1 account, you still need to specify the AWS account to use for each namespaces.

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/addons/tenants/tenant1/default/addons/multi-acct/values.yaml
   ```

   Values:

   ```yaml
   clusters:
      workload-cluster1: "012345678910" # AWS account for workload cluster 1
      workload-cluster2: "123456789101" # AWS account for workload cluster 2
   ```

   > We have configure our gitops tooling to uses of workload-clusterX namespace names, where X can go from 1 to 6, please don't change the names, just the values.
   > If you only want to use 1 AWS account, re-use the AWS account of your management cluster for the other workloads clusters

7. Add Commit and Push

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "initial commit"
   git push
   ```

### Create the Management cluster

1. update the `terraform.tfvars` with your values.

   Be sure to adapt the terraform.tfvars with your github working repo
   - git_org_name
   - update any of the gitops_xxx if you made any changes regarding the proposed setup (git path, branch...)
   - If you followed the instructions, the values for `gitops_xxx_repo_name` should be "eks-cluster-mgmt", update it to your git repo name if you modified it's name
   - configure `accounts_ids` with the list of AWS accounts you want to use for spoke clusters. If you want to create spoke clusters in the same management account, just put the management account id. This parameter is used for IAM roles configuration

   ```sh
   # edit: terraform.tfvars
   code $WORKSPACE_PATH/$WORKING_REPO/terraform/hub/terraform.tfvars
   ```

2. log-in into your aws management account.

   You need to connect to your AWS management account, I'm doing it with specific Profile, but this is up to your setup.
   ```sh
   export AWS_PROFILE=management_account # use your own profile or be sore to be connected to the appropriate account
   ```

3. Apply the terraform to create the management cluster:

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/terraform/hub
   ./install.sh
   ```

   review the changes and accept to deploy.

4. Connect to the cluster

   ```sh
   aws eks update-kubeconfig --name hub-cluster
   ```

5. Connect to the ArgoCD UI

   Execute the following command to get the Argocd UI url and password:

   ```sh
   echo "ArgoCD URL: http://$(kubectl get svc argocd-server -n argocd -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
    Login: admin
    Password: $(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d)"
   ```

6. If you are using private github repository, configure the repo with github token

   > We automate this with argocd cli, if you don't have it, you can simply to it in the Web interface in "Settings / Repositories"

   Authenticate with argocd cli:

   ```sh
   argocd login --grpc-web $(kubectl get svc argocd-server -n argocd -o jsonpath='{.status.loadBalancer.ingress[0].hostname}') --username admin --password $(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d) --insecure 
   ```

   Set a valid Github Token in GITHUB_TOKEN variable and then insert it in your argo cd instance:

   ```sh
   argocd repo add https://github.com/$GITHUB_ORG_NAME/$WORKING_REPO.git --username allamand --password $GITHUB_TOKEN --upsert --name github
   ```

   IF you have issue with your token you may have this error :
   > "Failed to load target state: failed to generate manifest for source 1 of 1: rpc error: code = Unknown desc = authentication required"

Once configure properly, Argo CD should start to install cluster-addons in the management cluster.

### Bootstrap Spoke accounts

In order for the management cluster to execute actions in the spoke AWS accounts, we need to create some IAM roles in the spoke accounts:

- `eks-cluster-mgmt-ec2`
- `eks-cluster-mgmt-eks`
- `eks-cluster-mgmt-iam`

> If you want to only test this in the management account, you still need to do this procedure, but replacing the list of spoke accounts number by the management account number.

We provided a script to help create thoses roles. You need to first connect to each of your spoke accounts and execute the script.

1. log-in into your aws Spoke account 2

   You need to connect to your AWS spoke account 1, I'm doing it with specific Profile, but adapt this to your own setup.

   ```sh
   export AWS_PROFILE=spoke_account1 # use your own profile or be sure to be connected to the appropriate account
   ```

2. Execute the script to configure IAM roles

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/scripts
   create_ack_workload_roles.sh
   ```

> Repeat this step for each of your Spoke accounts you want to use with the solution

### Create a Spoke cluster

Update $WORKSPACE_PATH/$WORKING_REPO

1. Add cluster creation by kro

   Edit the file :

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml
   ```

   Configure the AWS accounts for management and spoke account. There are some pre-requisites with thoses accounts when deploying the terraform code (it creates some IAM roles delegation).

   ```yaml
      workload-cluster1:
      managementAccountId: "012345678910" # replace the AWS account ID used for management cluster
      accountId: "123456789101" # replace the AWS account ID used for spoke workload cluster (It can be the same)
      tenant: "tenant1" # We have only configure tenant1 in the repo, If you change it, you need to duplicate all tenant1 directories
      k8sVersion: "1.30"
      gitops:
         addonsRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"    # replace the gitops repo in the file with your github account
         fleetRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
         platformRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
         workloadRepoUrl: "https://github.com/XXXXX/eks-cluster-mgmt"
   ```

2. Add Commit and Push

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "initial commit"
   git push
   ```

3. Restart kro to take into account new ACK CRD deployed by ArgoCD

   ```sh
   kubectl rollout restart deployment -n kro-system kro
   ```

4. Check that the ResourceGraph definition is properly reconciled and active, if not, restart agin kro like in previous 3.

   ```sh
   kubectl get resourcegraphdefinitions.kro.run
   ```

   Output expected:

   ```sh
   NAME                        APIVERSION   KIND                STATE    AGE
   ekscluster.kro.run          v1alpha1     EksCluster          Active   13m
   eksclusterwithvpc.kro.run   v1alpha1     EksclusterWithVpc   Active   12m
   vpc.kro.run                 v1alpha1     Vpc                 Active   13m
   ```

5. After some times, the cluster sould have been created in the spoke account.

   ```sh
   kubectl get EksClusterwithvpcs -A
   ```

   ```sh
   NAMESPACE   NAME                STATE    SYNCED   AGE
   argocd      workload-cluster1   ACTIVE   True     36m
   ```

   > If You see STATE=ERROR, that's may be normal as it will take some times for all dependencies to be OK, but you may want to see the logs of kro and ACK controllers in case you may have some configuration errors.

   You can also list resources created by kro to validate their satus:

   ```sh
   kubectl get vpcs.kro.run -A
   kubectl get vpcs.ec2.services.k8s.aws -A -o yaml # check there is not error
   ```

   > If you see errors, you may need to double check the multi-cluster accounts settings, and if IA roles in both management and workload aws accounts are properly configured.

   When VPC are ok, then check for EKS resources:

   ```sh
   kubectl get eksclusters.kro.run -A
   kubectl get clusters.eks.services.k8s.aws -A -o yaml # Check there are no errors
   ```

6. You can then connect to the spoke cluster

   ```sh
   export AWS_PROFILE=spoke_account1 # use your own profile or be sure to be connected to the appropriate account
   ```

   Get Kubectl configuration (update name and region if needed)

   ```sh
   aws eks update-kubeconfig --name workload-cluster1 --region us-west-2
   ```

   See what is deployed

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
   kube-sytem         efs-csi-controller-f7b568848-72rzc                  3/3     Running   0             4m12s
   kube-sytem         efs-csi-controller-f7b568848-vh85b                  3/3     Running   0             4m12s
   kube-sytem         efs-csi-node-5znbp                                  3/3     Running   0             4m13s
   kube-sytem         efs-csi-node-gzpsn                                  3/3     Running   0             4m13s
   kube-sytem         efs-csi-node-zbzlv                                  3/3     Running   0             4m13s
   kyverno            kyverno-admission-controller-5b4c74758b-kf2k7       1/1     Running   0             5m5s
   kyverno            kyverno-background-controller-7cf48d5b9d-f67h6      1/1     Running   0             5m5s
   kyverno            kyverno-cleanup-controller-cd4ccdd8c-4b4gp          1/1     Running   0             5m5s
   kyverno            kyverno-reports-controller-55c9f8d645-h8d57         1/1     Running   0             5m5s
   kyverno            policy-reporter-5c6c868c66-7jlxm                    1/1     Running   0             5m19s
   ```

   > In this case we can see that our gitops solution have deployed our addons in the cluster

7. Deploy workload example application

   If you want you can ask argo to syn the namespace application that will enable the workload deployment on the spoke cluster:

   ```sh
   argocd app sync namespaces-workload-cluster1-frontend
   argocd app sync namespaces-workload-cluster1-backend
   ```

   Wait a little for sync to proceed..

   ```sh
   kubectl get pods -A | egrep "carts|ui|rabbitmq|checkout|catalog|assets"
   ```

   ```sh
   assets             assets-6fd5c856d-chg7z                              1/1     Running   0              4m3s
   carts              carts-84cd5747cf-pnqg6                              1/1     Running   2 (4m8s ago)   4m46s
   carts              carts-dynamodb-6b64c98c4c-8mc86                     1/1     Running   0              4m4s
   catalog            catalog-5756744b6b-s4fzv                            1/1     Running   0              4m3s
   catalog            catalog-mysql-0                                     1/1     Running   0              5m44s
   checkout           checkout-d4c999847-jmlv5                            1/1     Running   0              4m3s
   checkout           checkout-redis-5c649558b6-stsvz                     1/1     Running   0              5m47s
   rabbitmq           rabbitmq-0                                          1/1     Running   0              5m45s
   ui                 ui-76c759877f-wgz69                                 1/1     Running   0              5m44s
   ```

   > We can now also see the namespaces for our application

## Conclusion

You can recreate thoses steps for any addional cluster you want to manage.

Each cluster is created by it's kro RGD, deployed to AWS using ACK controllers, and then automatically registered to Argo CD which can then install addons and workloads automatically.

## Clean-up

As our spoke workloads have been bootstrapped by Argo CD and kro/ACK, we need to clean them in specific order.

To be sure that I can delete the workload namespaces, there is 2 options: I can remove the Argo CD auto-sync or we can defined exception using the cluster name we want to delete

1. Check namespace application set configuration

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/fleet/bootstrap/namespaces-appset.yaml
   ```

2. Delete workload namespace in spoke cluster

   ```sh
   kubectl delete application -n argocd namespaces-workload-cluster1-backend
   kubectl delete application -n argocd namespaces-workload-cluster1-frontend
   ```

   So if our workload created some aws ressources like Load balancers, the Kubernetes addons, can clean thoses ressources properly.
   Once this is done, we can remove the spoke cluster creation from our management cluster

3. De-register spoke account

   Open the cluster configuration and comment the cluster you want to de-register

   ```sh
   code $WORKSPACE_PATH/$WORKING_REPO/fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml
   ```

4. Commit and Push

   ```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/
   git status
   git add .
   git commit -s -m "remove workload-cluster1"
   git push
   ```

5. Prune the cluster in Argo CD UI.

   Go to ArgoCD UI, and synchronise the cluster Applicationset clicking on the prune option to allow it for removal

6. Delete Management Cluster

Once you have successfuly de-register all your spoke accounts, you can remove the workload cluster created with Terraform

```sh
   cd $WORKSPACE_PATH/$WORKING_REPO/terraform/hub
   ./destroy.sh
```
