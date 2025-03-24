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
        local service="$1"
        cat <<EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "AWS": "arn:aws:iam::${MGMT_ACCOUNT_ID}:role/ack-${service}-controller-role-mgmt"
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

    #for SERVICE in iam ec2 eks secretsmanager; do
    for SERVICE in iam ec2 eks; do
        echo ">>>>>>>>>SERVICE:$SERVICE"
        local ROLE_NAME="eks-cluster-mgmt-${SERVICE}"

        # # First, detach any managed policies
        # for policy in $(aws iam list-attached-role-policies --role-name "${ROLE_NAME}" --query 'AttachedPolicies[*].PolicyArn' --output text 2>/dev/null); do
        #     echo "Detaching policy $policy from role ${ROLE_NAME}"
        #     aws iam detach-role-policy --role-name "${ROLE_NAME}" --policy-arn "$policy"
        # done

        # # Delete any inline policies
        # for policy in $(aws iam list-role-policies --role-name "${ROLE_NAME}" --query 'PolicyNames[*]' --output text 2>/dev/null); do
        #     echo "Deleting inline policy $policy from role ${ROLE_NAME}"
        #     aws iam delete-role-policy --role-name "${ROLE_NAME}" --policy-name "$policy"
        # done

        # # Delete the role if it exists
        # aws iam delete-role --role-name "${ROLE_NAME}" 2>/dev/null
        
        # Generate the trust policy for this service
        local TRUST_POLICY
        TRUST_POLICY=$(generate_trust_policy "$SERVICE")
        echo "${TRUST_POLICY}" > trust-${SERVICE}.json

        # Create the role with the trust policy
        echo "Creating role ${ROLE_NAME}"
        local ROLE_DESCRIPTION="Workload role for ACK ${SERVICE} controller"
        if ! aws iam create-role \
            --role-name "${ROLE_NAME}" \
            --assume-role-policy-document file://trust-${SERVICE}.json \
            --description "${ROLE_DESCRIPTION}"; then
            echo "Role ${ROLE_NAME} created"
            rm -f trust-${SERVICE}.json
            continue
        fi

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
                --policy-name "ack-recommended-policy" \
                --policy-document "$INLINE_POLICY"
            echo "ok."
        fi

        if [ $? -eq 0 ]; then
            echo "Successfully created and configured role ${ROLE_NAME}"
            local ROLE_ARN
            ROLE_ARN=$(aws iam get-role --role-name "${ROLE_NAME}" --query Role.Arn --output text)
            echo "Role ARN: ${ROLE_ARN}"
        else
            echo "Failed to create/configure role ${ROLE_NAME}"
            return 1
        fi

        # Cleanup
        rm -f trust-${SERVICE}.json
    done

    return 0
}

# Main script execution
if [ ! -z "$MGMT_ACCOUNT_ID" ]; then
    echo "Management Account ID: $MGMT_ACCOUNT_ID"
    create_ack_workload_roles "$MGMT_ACCOUNT_ID"
else
    echo "You must set the MGMT_ACCOUNT_ID environment variable"
    echo "Example: export MGMT_ACCOUNT_ID=123456789012"
    exit 1
fi