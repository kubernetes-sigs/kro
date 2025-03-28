#!/bin/bash

# Script to delete IAM roles by first removing all attached policies
# Usage: ./delete_ack_workload_roles.sh role1 role2 role3 ...
# ./delete_ack_workload_roles.sh eks-cluster-mgmt-iam eks-cluster-mgmt-ec2 eks-cluster-mgmt-eks

set -e

# Check if AWS CLI is installed
if ! command -v aws &> /dev/null; then
    echo "AWS CLI is not installed. Please install it first."
    exit 1
fi

# Check if at least one role name is provided
if [ $# -eq 0 ]; then
    echo "Usage: $0 role1 role2 role3 ..."
    echo "Please provide at least one role name to delete."
    exit 1
fi

# Function to delete a role
delete_role() {
    local role_name=$1
    echo "Processing role: $role_name"
    
    # Check if role exists
    if ! aws iam get-role --role-name "$role_name" &> /dev/null; then
        echo "Role $role_name does not exist. Skipping."
        return 0
    fi
    
    # List and detach managed policies
    echo "Checking for attached managed policies..."
    local attached_policies=$(aws iam list-attached-role-policies --role-name "$role_name" --query "AttachedPolicies[*].PolicyArn" --output text)
    
    if [ -n "$attached_policies" ]; then
        echo "Detaching managed policies from $role_name..."
        for policy_arn in $attached_policies; do
            echo "  Detaching policy: $policy_arn"
            aws iam detach-role-policy --role-name "$role_name" --policy-arn "$policy_arn"
        done
    else
        echo "No managed policies attached to $role_name."
    fi
    
    # List and delete inline policies
    echo "Checking for inline policies..."
    local inline_policies=$(aws iam list-role-policies --role-name "$role_name" --query "PolicyNames" --output text)
    
    if [ -n "$inline_policies" ] && [ "$inline_policies" != "None" ]; then
        echo "Removing inline policies from $role_name..."
        for policy_name in $inline_policies; do
            echo "  Removing inline policy: $policy_name"
            aws iam delete-role-policy --role-name "$role_name" --policy-name "$policy_name"
        done
    else
        echo "No inline policies for $role_name."
    fi
    
    # Delete instance profiles associated with the role (if any)
    echo "Checking for instance profiles..."
    local instance_profiles=$(aws iam list-instance-profiles-for-role --role-name "$role_name" --query "InstanceProfiles[*].InstanceProfileName" --output text)
    
    if [ -n "$instance_profiles" ] && [ "$instance_profiles" != "None" ]; then
        echo "Removing role from instance profiles..."
        for profile_name in $instance_profiles; do
            echo "  Removing role from instance profile: $profile_name"
            aws iam remove-role-from-instance-profile --instance-profile-name "$profile_name" --role-name "$role_name"
        done
    else
        echo "No instance profiles for $role_name."
    fi
    
    # Finally delete the role
    echo "Deleting role: $role_name"
    aws iam delete-role --role-name "$role_name"
    echo "Role $role_name successfully deleted."
    echo "----------------------------------------"
}

# Process each role
for role in "$@"; do
    delete_role "$role"
done

echo "All specified roles have been processed."
