################################################################################
# External Secrets EKS Access
################################################################################
module "external_secrets_pod_identity" {
  count   = local.aws_addons.enable_external_secrets ? 1 : 0
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 1.4.0"

  name = "external-secrets"

  attach_external_secrets_policy        = true
  external_secrets_kms_key_arns         = ["arn:aws:kms:${local.region}:*:key/${local.cluster_info.cluster_name}/*"]
  external_secrets_secrets_manager_arns = ["arn:aws:secretsmanager:${local.region}:*:secret:${local.cluster_info.cluster_name}/*"]
  external_secrets_ssm_parameter_arns   = ["arn:aws:ssm:${local.region}:*:parameter/${local.cluster_info.cluster_name}/*"]
  external_secrets_create_permission    = false
  attach_custom_policy                  = true
  policy_statements = [
    {
      sid       = "ecr"
      actions   = ["ecr:*"]
      resources = ["*"]
    }
  ]
  # Pod Identity Associations
  associations = {
    addon = {
      cluster_name    = local.cluster_info.cluster_name
      namespace       = local.external_secrets.namespace
      service_account = local.external_secrets.service_account
    }
  }

  tags = local.tags
}
