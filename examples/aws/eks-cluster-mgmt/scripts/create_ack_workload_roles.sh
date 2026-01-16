#!/bin/bash

# Disable AWS CLI paging
export AWS_PAGER=""

create_ack_workload_roles() {
    local MGMT_ACCOUNT_ID="$1"

    if [ -z "$MGMT_ACCOUNT_ID" ]; then
        echo "Usage: create_ack_workload_roles <mgmt-account-id>"
        echo "Example: create_ack_workload_roles 123456789012"
        return 1
    fi
    # Generate trust policy for a specific service
    generate_trust_policy() {
        cat <<EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "AWS": "arn:aws:iam::${MGMT_ACCOUNT_ID}:role/${CLUSTER_NAME}-ack-controller"
            },
            "Action": [
              "sts:AssumeRole",
              "sts:TagSession"
              ],
            "Condition": {}
        }
    ]
}
EOF
    }

    # Generate the trust policy for this service
    local TRUST_POLICY
    TRUST_POLICY=$(generate_trust_policy)
    echo "${TRUST_POLICY}" > trust.json


    # Create the role with the trust policy
    local ROLE_NAME="ack"
    local ROLE_DESCRIPTION="Workload role for ACK controllers"
    echo "Creating role ${ROLE_NAME}"
    aws iam create-role \
        --role-name "${ROLE_NAME}" \
        --assume-role-policy-document file://trust.json \
        --description "${ROLE_DESCRIPTION}"

    if [ $? -eq 0 ]; then
        echo "Successfully created role ${ROLE_NAME}"
        local ROLE_ARN
        ROLE_ARN=$(aws iam get-role --role-name "${ROLE_NAME}" --query Role.Arn --output text)
        echo "Role ARN: ${ROLE_ARN}"
        rm -f trust.json
    else
        echo "Failed to create/configure role ${ROLE_NAME}"
        rm -f trust.json
        return 1
    fi

    #for SERVICE in iam ec2 eks secretsmanager; do
    for SERVICE in iam ec2 eks; do
        echo ">>>>>>>>>SERVICE:$SERVICE"

        # Download and apply the recommended policies
        local BASE_URL="https://raw.githubusercontent.com/aws-controllers-k8s/${SERVICE}-controller/main"
        local POLICY_ARN_URL="${BASE_URL}/config/iam/recommended-policy-arn"
        local POLICY_ARN_STRINGS
        POLICY_ARN_STRINGS="$(wget -qO- ${POLICY_ARN_URL})"

        local INLINE_POLICY_URL="${BASE_URL}/config/iam/recommended-inline-policy"
        local INLINE_POLICY
        INLINE_POLICY="$(wget -qO- ${INLINE_POLICY_URL})"

        # Attach managed policies
        while IFS= read -r POLICY_ARN; do
            if [ -n "$POLICY_ARN" ]; then
                echo -n "Attaching $POLICY_ARN ... "
                aws iam attach-role-policy \
                    --role-name "${ROLE_NAME}" \
                    --policy-arn "${POLICY_ARN}"
                echo "ok."
            fi
        done <<< "$POLICY_ARN_STRINGS"

        # Add inline policy if it exists
        if [ ! -z "$INLINE_POLICY" ]; then
            echo -n "Putting inline policy ... "
            aws iam put-role-policy \
                --role-name "${ROLE_NAME}" \
                --policy-name "ack-recommended-policy-${SERVICE}" \
                --policy-document "$INLINE_POLICY"
            echo "ok."
        fi

        if [ $? -eq 0 ]; then
            echo "Successfully configured role ${ROLE_NAME}"
        else
            echo "Failed to configure role ${ROLE_NAME}"
            return 1
        fi
    done

    return 0
}

# Main script execution
if [ -z "$MGMT_ACCOUNT_ID" ]; then
    echo "You must set the MGMT_ACCOUNT_ID environment variable"
    echo "Example: export MGMT_ACCOUNT_ID=123456789012"
    exit 1
fi

if [ -z "$CLUSTER_NAME" ]; then
    echo "You must set the CLUSTER_NAME environment variable"
    echo "Example: export CLUSTER_NAME=hub-cluster"
    exit 1
fi

echo "Management Account ID: $MGMT_ACCOUNT_ID"
echo "Cluster Name: $CLUSTER_NAME"
create_ack_workload_roles "$MGMT_ACCOUNT_ID"