#!/bin/bash

create_external_secret_role() {
    local CLUSTER_NAME="$1"

    if [ -z "$CLUSTER_NAME" ]; then
        echo "Usage: create_external_secret_role <cluster_name>"
        return 1
    fi

    echo ">>>>>>>>>Creating External Secrets Role"

    # 1. Create the IAM role for pod identity
    local TRUST_RELATIONSHIP='{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Sid": "AllowEksAuthToAssumeRoleForPodIdentity",
                "Effect": "Allow",
                "Principal": {
                    "Service": "pods.eks.amazonaws.com"
                },
                "Action": [
                    "sts:AssumeRole",
                    "sts:TagSession"
                ]
            }
        ]
    }'

    echo "${TRUST_RELATIONSHIP}" > trust.json

    aws iam create-role \
        --role-name external-secrets \
        --assume-role-policy-document file://trust.json

    # 2. Create IAM policy for External Secrets access
    local POLICY_DOCUMENT='{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": [
                    "ssm:GetParameter*",
                    "ssm:DescribeParameters"
                ],
                "Resource": "arn:aws:ssm:*:*:parameter/*"
            },
            {
                "Effect": "Allow",
                "Action": [
                    "secretsmanager:GetSecretValue",
                    "secretsmanager:DescribeSecret"
                ],
                "Resource": "arn:aws:secretsmanager:*:*:secret:*"
            },
            {
                "Effect": "Allow",
                "Action": [
                    "kms:Decrypt",
                    "kms:DescribeKey"
                ],
                "Resource": "arn:aws:kms:*:*:key/*"
            }
        ]
    }'

    aws iam create-policy \
        --policy-name external-secrets-policy \
        --policy-document "$POLICY_DOCUMENT"

    # 3. Attach the policy to the role
    local POLICY_ARN
    POLICY_ARN=$(aws iam list-policies --query 'Policies[?PolicyName==`external-secrets-policy`].Arn' --output text)
    
    aws iam attach-role-policy \
        --role-name external-secrets \
        --policy-arn "$POLICY_ARN"

    # 4. Create the pod identity association
    local ROLE_ARN
    ROLE_ARN=$(aws iam get-role --role-name external-secrets --query 'Role.Arn' --output text)

    aws eks create-pod-identity-association \
        --cluster-name "$CLUSTER_NAME" \
        --namespace "external-secrets" \
        --service-account "external-secrets" \
        --role-arn "$ROLE_ARN"

    echo "<<<<<<<<<< External Secrets Role Created"
    return 0
}

create_ack_roles() {
    local CLUSTER_NAME="$1"
    local ACCOUNT_IDS="$2"

    if [ -z "$CLUSTER_NAME" ] || [ -z "$ACCOUNT_IDS" ]; then
        echo "Usage: create_ack_roles <cluster_name> <account_ids>"
        echo "Example: create_ack_roles my-cluster '123456789012 987654321098'"
        return 1
    fi

    # Helper function to generate cross-account policy
    generate_policy() {
        local service="$1"
        local resource_arns=""
        for account in $ACCOUNT_IDS; do
            if [ -n "$resource_arns" ]; then
                resource_arns="${resource_arns},"
            fi
            resource_arns="${resource_arns}\"arn:aws:iam::${account}:role/eks-cluster-mgmt-$service\""
        done

        cat <<EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "sts:AssumeRole",
                "sts:TagSession"
            ],
            "Resource": [
                ${resource_arns}
            ]
        }
    ]
}
EOF
    }

    for SERVICE in iam ec2 eks; do
        echo ">>>>>>>>>SERVICE:$SERVICE"
        local ACK_CONTROLLER_IAM_ROLE="ack-${SERVICE}-controller"
        
        local ACK_K8S_NAMESPACE=ack-system
        local ACK_K8S_SERVICE_ACCOUNT_NAME=ack-$SERVICE-controller

        local TRUST_RELATIONSHIP='{
            "Version": "2012-10-17",
            "Statement": [
                {
                    "Sid": "AllowEksAuthToAssumeRoleForPodIdentity",
                    "Effect": "Allow",
                    "Principal": {
                        "Service": "pods.eks.amazonaws.com"
                    },
                    "Action": [
                        "sts:AssumeRole",
                        "sts:TagSession"
                    ]
                }
            ]
        }'

        echo "${TRUST_RELATIONSHIP}" > trust.json

        local ACK_CONTROLLER_IAM_ROLE_DESCRIPTION="IRSA role for ACK ${SERVICE} controller deployment on EKS cluster using Helm charts"
        aws iam create-role \
            --role-name "${ACK_CONTROLLER_IAM_ROLE}" \
            --assume-role-policy-document file://trust.json \
            --description "${ACK_CONTROLLER_IAM_ROLE_DESCRIPTION}"

        local ACK_CONTROLLER_IAM_ROLE_ARN
        ACK_CONTROLLER_IAM_ROLE_ARN=$(aws iam get-role --role-name=$ACK_CONTROLLER_IAM_ROLE --query Role.Arn --output text)

        # Download and apply policies
        local BASE_URL="https://raw.githubusercontent.com/aws-controllers-k8s/${SERVICE}-controller/main"
        local POLICY_ARN_URL="${BASE_URL}/config/iam/recommended-policy-arn"
        local POLICY_ARN_STRINGS
        POLICY_ARN_STRINGS="$(wget -qO- ${POLICY_ARN_URL})"

        local INLINE_POLICY_URL="${BASE_URL}/config/iam/recommended-inline-policy"
        local INLINE_POLICY
        INLINE_POLICY="$(wget -qO- ${INLINE_POLICY_URL})"

        while IFS= read -r POLICY_ARN; do
            if [ -n "$POLICY_ARN" ]; then
                echo -n "Attaching $POLICY_ARN ... "
                aws iam attach-role-policy \
                    --role-name "${ACK_CONTROLLER_IAM_ROLE}" \
                    --policy-arn "${POLICY_ARN}"
                echo "ok."
            fi
        done <<< "$POLICY_ARN_STRINGS"

        if [ ! -z "$INLINE_POLICY" ]; then
            echo -n "Putting inline policy ... "
            aws iam put-role-policy \
                --role-name "${ACK_CONTROLLER_IAM_ROLE}" \
                --policy-name "ack-recommended-policy" \
                --policy-document "$INLINE_POLICY"
            echo "ok."
        fi

        # Generate and apply cross-account policy
        local CROSS_ACCOUNT_POLICY
        CROSS_ACCOUNT_POLICY=$(generate_policy $SERVICE)

        echo -n "Putting cross-account inline policy... "
        aws iam put-role-policy \
            --role-name "${ACK_CONTROLLER_IAM_ROLE}" \
            --policy-name "cross-account-access" \
            --policy-document "$CROSS_ACCOUNT_POLICY"
        
        if [ $? -eq 0 ]; then
            echo "ok."
        else
            echo "failed!"
            return 1
        fi

        aws eks create-pod-identity-association \
            --cluster-name "$CLUSTER_NAME" \
            --role-arn "$ACK_CONTROLLER_IAM_ROLE_ARN" \
            --namespace "$ACK_K8S_NAMESPACE" \
            --service-account "$ACK_K8S_SERVICE_ACCOUNT_NAME"

        echo "<<<<<<<<<< $ACK_CONTROLLER_IAM_ROLE_ARN"
    done

    rm -f trust.json
    return 0
}

if [[ ! -z "$CLUSTER_NAME" && ! -z "$ACCOUNT_IDS" ]]; then
    echo "CLUSTER_NAME: $CLUSTER_NAME"
    echo "ACCOUNT_IDS: $ACCOUNT_IDS"
    #create_ack_roles "$CLUSTER_NAME" "$ACCOUNT_IDS"
    echo "proute"
    create_external_secret_role "$CLUSTER_NAME"
else
    echo "You must configure the environments variables first"
    echo "CLUSTER_NAME: $CLUSTER_NAME"
    echo "ACCOUNT_IDS: $ACCOUNT_IDS"
    exit -1
fi