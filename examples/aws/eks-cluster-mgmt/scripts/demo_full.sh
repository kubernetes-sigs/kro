 #!/usr/bin/env bash
set -euo pipefail
SCRIPTPATH="$( cd "$(dirname "$0")" ; pwd -P )"
source "${SCRIPTPATH}/lib/utils.sh"
#CLUSTER_NAME=$EKS_CLUSTER_NAME
FILEPATH=/tmp/demo-kubecon

#eval $(/opt/homebrew/bin/isengardcli credentials sallaman+demo3@amazon.com --role Admin --region $AWS_REGION )
eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-mgmt@amazon.fr --role Admin --region $AWS_REGION )

alias code='/Applications/Visual\ Studio\ Code.app/Contents/MacOS/Electron'


mkdir -p $FILEPATH

# clear
# figlet "Welcome to KubeCon EU 2025" | lolcat
# sleep 1
# echo ""
# echo "Creating and managing Amazon EKS clusters with kro and ACK" | lolcat
# read -n 1

# echo "## 1.) Terraform configuration used:" | lolcat
# cmd "code terraform/hub/kubecon.tfvars"
# direnv allow
# prompt "# "

# echo "## 2.) Deploy the EKS management cluster using terraform" | lolcat
# cd terraform/hub
# cmd "terraform apply -var-file=$TF_VAR_FILE --auto-approve"
# cd -
# prompt "# "

# clear
# aws eks update-kubeconfig --name hub-cluster --region $AWS_REGION
# export ARGOCD_SERVER=$(kubectl get svc argocd-server -n argocd -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
# export ARGOCD_PWD=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d)
# argocd_cred(){
#   echo "ArgoCD URL: http://$ARGOCD_SERVER
#     Username: admin
#     Password: XXXXXXX"
# }
# echo "## 3.) Retrieve ArgoCD Credentials" | lolcat
# cmd "argocd_cred"
# prompt "# "

# echo "## 4.) Open ArgoCD UI" | lolcat
# cmd "open http://$ARGOCD_SERVER/applications/argocd/cluster-application-sets?view=tree&resource="
# prompt "# "

# echo "## 5.) Authenticate with argo CLI " | lolcat
# cmd "echo argocd login --grpc-web $ARGOCD_SERVER --username admin --password XXXXXX --insecure "
# argocd login --grpc-web $ARGOCD_SERVER --username admin --password $ARGOCD_PWD --insecure
# prompt "# "

# echo "## 6.) Add Git Private repo to Argo CD UI " | lolcat
# cmd "echo argocd repo add https://github.com/allamand/gitops-fleet-management.git --username allamand --password XXXXXXX --upsert --name kubecon"
# argocd repo add https://github.com/allamand/gitops-fleet-management.git --username allamand --password $GITHUB_TOKEN --upsert --name kubecon
# prompt "# "

# echo "## 7.) Check repo servers " | lolcat
# cmd "open http://$ARGOCD_SERVER/settings/repos"
# prompt "# "

# clear
# echo "## 8.) let's discover AWS workload account" | lolcat
# cmd "open https://eu-west-2.console.aws.amazon.com/console/home?region=eu-west-2#"
# prompt "# "

# clear
# cmd "echo Connecting to Workload1..."

# #isengardcli assume sallaman+kro-c1@amazon.fr --role Admin --region $AWS_REGION
# eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-c1@amazon.fr --role Admin --region $AWS_REGION )
# echo "## 9.) Create pre-requisites IAM roles in Spoke cluster" | lolcat

# export MGMT_ACCOUNT_ID=515966522948
# aws iam create-service-linked-role --aws-service-name eks.amazonaws.com || true
# aws iam create-service-linked-role --aws-service-name ecr.amazonaws.com || true
# cmd "sh ./scripts/create_ack_workload_roles.sh"
# prompt "# "

# echo "## 8.) Check again Spoke account Iam Roles" | lolcat
# cmd "open https://586794472760-jrlzxg2n.us-east-1.console.aws.amazon.com/iam/home?region=eu-west-2#/roles"
# prompt "# "

# clear
# cmd "echo Connecting to Workload3..."

# #isengardcli assume sallaman+kro-c1@amazon.fr --role Admin --region $AWS_REGION
# eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-c2@amazon.fr --role Admin --region $AWS_REGION )
# echo "## 9.) Create pre-requisites IAM roles in Spoke cluster" | lolcat

# export MGMT_ACCOUNT_ID=515966522948
# aws iam create-service-linked-role --aws-service-name eks.amazonaws.com || true
# aws iam create-service-linked-role --aws-service-name ecr.amazonaws.com || true
# cmd "sh ./scripts/create_ack_workload_roles.sh"
# prompt "# "

# echo "## 8.) Check again Spoke account Iam Roles" | lolcat
# cmd "open https://825765380480-uafl4nz7.us-east-1.console.aws.amazon.com/iam/home?region=eu-west-2#/roles"
# prompt "# "

# clear
# figlet "Creating EKS cluster in Spoke account 1" | lolcat

# echo "## 1.) Look at Kro ResourceGraphDefinition Creation" | lolcat
# cmd "code charts/kro/resource-groups/eks/rg-eks-vpc.yaml"
# prompt "# "

# echo "## 2.) Look at Kro EksclusterWithVpc CustomResource Helm Chart" | lolcat
# cmd "code charts/kro-clusters/templates/clusters.yaml"
# prompt "# "

# echo "## 3.) Create workload cluster1 in account 1 for our tenant tenant1" | lolcat
# cmd "code fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml"
# prompt "# "
# cmd "git status"
# cmd "git add ."
# cmd "git commit -s -m 'add workload-cluster1'" 
# cmd "git push seb"
# prompt "# "

# echo "## 4.) Watch cluster creation in ArgoCD" | lolcat
# cmd "open https://k8s-argocd-argocdse-0144e2a813-c176fbf08a6b1e43.elb.eu-west-2.amazonaws.com/applications/argocd/cluster-application-sets?view=tree&resource="
# prompt "# "

# clear
# figlet "Creating EKS cluster in Spoke account 2" | lolcat

# echo "## 1.) Create workload cluster2 in account 2 for our tenant tenant1" | lolcat
# cmd "code fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml"
# prompt "# "
# cmd "git status"
# cmd "git add ."
# cmd "git commit -s -m 'add workload-cluster2'" 
# cmd "git push seb"
# prompt "# "

# echo "## 4.) Watch cluster creation in ArgoCD" | lolcat
# cmd "open https://k8s-argocd-argocdse-0144e2a813-c176fbf08a6b1e43.elb.eu-west-2.amazonaws.com/applications/argocd/cluster-application-sets?view=tree&resource="
# prompt "# "

clear
cmd "## THANKS YOU" | lolcat
figlet "Go Build, Try KRO and ACK!!" | lolcat




