 #!/usr/bin/env bash
set -euo pipefail
SCRIPTPATH="$( cd "$(dirname "$0")" ; pwd -P )"
source "${SCRIPTPATH}/lib/utils.sh"


FILEPATH=/tmp/demo-kubecon
mkdir -p $FILEPATH

CLUSTER_NAME=hub-cluster

#eval $(/opt/homebrew/bin/isengardcli credentials sallaman+demo3@amazon.com --role Admin --region $AWS_REGION )
eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-mgmt@amazon.fr --role Admin --region $AWS_REGION )

aws eks update-kubeconfig --name $CLUSTER_NAME --region $AWS_REGION
export ARGOCD_SERVER=$(kubectl get svc argocd-server -n argocd -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
export ARGOCD_PWD=$(kubectl -n argocd get secret argocd-initial-admin-secret -o jsonpath="{.data.password}" | base64 -d)

#argocd login --grpc-web $ARGOCD_SERVER --username admin --password $ARGOCD_PWD --insecure
#argocd repo add https://github.com/allamand/gitops-fleet-management.git --username allamand --password $GITHUB_TOKEN --upsert --name kubecon

#If needed bootstrap spoke cluster
# eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-c1@amazon.fr --role Admin --region $AWS_REGION )
# aws iam create-service-linked-role --aws-service-name eks.amazonaws.com || true
# aws iam create-service-linked-role --aws-service-name ecr.amazonaws.com || true
# cmd "sh ./scripts/create_ack_workload_roles.sh"

alias code='/Applications/Visual\ Studio\ Code.app/Contents/MacOS/Electron'

###############################################
# Start demo script
###############################################


clear
figlet "Welcome to KubeCon EU 2025" | lolcat
sleep 1
echo ""
cmd "## Creating and managing Amazon EKS clusters with kro and ACK" | lolcat
read -n 1


cmd "## Open Management Cluster ArgoCD UI" | lolcat
cmd "open http://$ARGOCD_SERVER/applications/argocd/bootstrap?view=tree&resource="
prompt "# "

cmd "## Open workload cluster 1 EKS console" | lolcat
cmd "open https://586794472760-jrlzxg2n.us-west-2.console.aws.amazon.com/eks/clusters?region=us-west-2"
prompt "# "

clear
figlet "Creating EKS cluster in Spoke account 1" | lolcat

cmd "## Create workload cluster1 in account 1 for our tenant tenant1" | lolcat
cmd "code fleet/kro-values/tenants/tenant1/kro-clusters/values.yaml"
prompt "# "
cmd "git status && git add ."
cmd "git commit -s -m 'add workload-cluster1' && git push seb" 
prompt "# "

cmd "## Watch cluster creation in ArgoCD" | lolcat
cmd "open https://k8s-argocd-argocdse-0144e2a813-c176fbf08a6b1e43.elb.eu-west-2.amazonaws.com/applications/argocd/bootstrap?view=tree&resource="
prompt "# "

clear
cmd "## Once Created connect to workload-cluster1 EKS cluster" | lolcat

#eval export AWS_PROFILE=sallaman+kro-c1-Admin && 
eval $(/opt/homebrew/bin/isengardcli credentials sallaman+kro-c1@amazon.fr --role Admin --region $AWS_REGION )
aws eks update-kubeconfig --name workload-cluster1 --region us-west-2

#kubectl rollout restart deployment -n ack-system eks-chart
#kubectl rollout restart deployment -n ack-system iam-chart
#prompt "# "
cmd "open https://k8s-argocd-argocdse-0144e2a813-c176fbf08a6b1e43.elb.eu-west-2.amazonaws.com/applications/argocd/bootstrap?view=tree&resource="
prompt "# "

cmd "## Add Workload Application" | lolcat
sed -i '' 's/#- staging/- staging/' fleet/bootstrap/namespaces-appset.yaml
sed -i '' 's/#- staging/- staging/' fleet/bootstrap/web-store-backend-appset.yaml
sed -i '' 's/#- staging/- staging/' fleet/bootstrap/web-store-frontend-appset.yaml
cmd "code fleet/bootstrap/namespaces-appset.yaml"
cmd "git status && git add ."
cmd "git commit -s -m 'add workload-cluster1' && git push seb" 
prompt "# "

clear
prompt "## . "
clear
cmd "## THANKS YOU" | lolcat
figlet "Go Build, Try kro and ACK!!" | lolcat




