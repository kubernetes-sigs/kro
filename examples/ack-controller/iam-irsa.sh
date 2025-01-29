#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -e

# Default values
EKS_CLUSTER_NAME=${EKS_CLUSTER_NAME:-curious-folk-badger}
AWS_REGION=${AWS_REGION:-us-west-2}
ACK_K8S_NAMESPACE=${ACK_K8S_NAMESPACE:-kro}
ACK_K8S_SERVICE_ACCOUNT_NAME=${ACK_K8S_SERVICE_ACCOUNT_NAME:-ack-iam-controller-sa}

# Function to check if a command was successful
check_success() {
    if [ $? -eq 0 ]; then
        echo "Success: $1"
    else
        echo "Error: $1 failed"
        exit 1
    fi
}

# Function to display usage
usage() {
    echo "Usage: $0 [OPTIONS]"
    echo "Options:"
    echo "  --cluster-name NAME       Set EKS cluster name (default: $EKS_CLUSTER_NAME)"
    echo "  --region REGION           Set AWS region (default: $AWS_REGION)"
    echo "  --namespace NAMESPACE     Set Kubernetes namespace (default: $ACK_K8S_NAMESPACE)"
    echo "  --service-account NAME    Set service account name (default: $ACK_K8S_SERVICE_ACCOUNT_NAME)"
    echo "  --help                    Display this help message"
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    key="$1"
    case $key in
        --cluster-name)
        EKS_CLUSTER_NAME="$2"
        shift 2
        ;;
        --region)
        AWS_REGION="$2"
        shift 2
        ;;
        --namespace)
        ACK_K8S_NAMESPACE="$2"
        shift 2
        ;;
        --service-account)
        ACK_K8S_SERVICE_ACCOUNT_NAME="$2"
        shift 2
        ;;
        --help)
        usage
        exit 0
        ;;
        *)
        echo "Unknown option: $1"
        usage
        exit 1
        ;;
    esac
done

echo "Using the following settings:"
echo "EKS Cluster Name: $EKS_CLUSTER_NAME"
echo "AWS Region: $AWS_REGION"
echo "Kubernetes Namespace: $ACK_K8S_NAMESPACE"
echo "Service Account Name: $ACK_K8S_SERVICE_ACCOUNT_NAME"

# Update kubeconfig
aws eks update-kubeconfig --region $AWS_REGION --name $EKS_CLUSTER_NAME
check_success "Updating kubeconfig"

# Create service account
kubectl create sa -n $ACK_K8S_NAMESPACE $ACK_K8S_SERVICE_ACCOUNT_NAME
check_success "Creating service account"

# Get AWS account ID
AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query "Account" --output text)
check_success "Getting AWS account ID"

# Get OIDC provider
OIDC_PROVIDER=$(aws eks describe-cluster --name $EKS_CLUSTER_NAME --region $AWS_REGION --query "cluster.identity.oidc.issuer" --output text | sed -e "s/^https:\/\///")
check_success "Getting OIDC provider"

# Create trust relationship JSON
cat << EOF > trust.json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "${OIDC_PROVIDER}:sub": "system:serviceaccount:${ACK_K8S_NAMESPACE}:${ACK_K8S_SERVICE_ACCOUNT_NAME}"
        }
      }
    }
  ]
}
EOF
check_success "Creating trust relationship JSON"

# Create IAM role
ACK_CONTROLLER_IAM_ROLE="ack-iam-controller"
ACK_CONTROLLER_IAM_ROLE_DESCRIPTION="IRSA role for ACK IAM controller deployment on EKS cluster using Helm charts"
aws iam create-role --role-name "${ACK_CONTROLLER_IAM_ROLE}" --assume-role-policy-document file://trust.json --description "${ACK_CONTROLLER_IAM_ROLE_DESCRIPTION}"
check_success "Creating IAM role"

# Get IAM role ARN
ACK_CONTROLLER_IAM_ROLE_ARN=$(aws iam get-role --role-name=$ACK_CONTROLLER_IAM_ROLE --query Role.Arn --output text)
check_success "Getting IAM role ARN"

# Download and apply policies
BASE_URL=https://raw.githubusercontent.com/aws-controllers-k8s/iam-controller/main
POLICY_ARN_URL=${BASE_URL}/config/iam/recommended-policy-arn
POLICY_ARN_STRINGS="$(wget -qO- ${POLICY_ARN_URL})"
check_success "Downloading recommended policy ARNs"

INLINE_POLICY_URL=${BASE_URL}/config/iam/recommended-inline-policy
INLINE_POLICY="$(wget -qO- ${INLINE_POLICY_URL})"
check_success "Downloading recommended inline policy"

# Attach managed policies
while IFS= read -r POLICY_ARN; do
    aws iam attach-role-policy --role-name "${ACK_CONTROLLER_IAM_ROLE}" --policy-arn "${POLICY_ARN}"
    check_success "Attaching policy ${POLICY_ARN}"
done <<< "$POLICY_ARN_STRINGS"

# Put inline policy if it exists
if [ ! -z "$INLINE_POLICY" ]; then
    aws iam put-role-policy --role-name "${ACK_CONTROLLER_IAM_ROLE}" --policy-name "ack-iam-recommended-policy" --policy-document "$INLINE_POLICY"
    check_success "Putting inline policy"
fi

# Annotate the service account with the ARN
IRSA_ROLE_ARN=eks.amazonaws.com/role-arn=$ACK_CONTROLLER_IAM_ROLE_ARN
kubectl annotate serviceaccount -n $ACK_K8S_NAMESPACE $ACK_K8S_SERVICE_ACCOUNT_NAME $IRSA_ROLE_ARN
check_success "Annotating service account"

echo "Script completed successfully"
