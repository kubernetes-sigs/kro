locals {
  cluster_info              = module.eks
  vpc_cidr                  = "10.0.0.0/16"
  azs                       = slice(data.aws_availability_zones.available.names, 0, 2)
  enable_automode           = var.enable_automode
  use_ack                   = var.use_ack
  enable_efs                = var.enable_efs
  name                      = var.cluster_name
  environment               = var.environment
  fleet_member               = "control-plane"
  tenant                    = var.tenant
  region                    = data.aws_region.current.id
  cluster_version           = var.kubernetes_version
  argocd_namespace          = "argocd"
  gitops_addons_repo_url    = "https://github.com/${var.git_org_name}/${var.gitops_addons_repo_name}.git"
  gitops_fleet_repo_url     = "https://github.com/${var.git_org_name}/${var.gitops_fleet_repo_name}.git"

  external_secrets = {
    namespace       = "external-secrets"
    service_account = "external-secrets-sa"
  }

  aws_addons = {
    enable_external_secrets                      = try(var.addons.enable_external_secrets, false)
    enable_kro_eks_rgs                           = try(var.addons.enable_kro_eks_rgs, false)
    enable_multi_acct                            = try(var.addons.enable_multi_acct, false)
  }
  oss_addons = {
  }

  addons = merge(
    local.aws_addons,
    local.oss_addons,
    { tenant = local.tenant },
    { fleet_member = local.fleet_member },
    { kubernetes_version = local.cluster_version },
    { aws_cluster_name = local.cluster_info.cluster_name },
  )

  addons_metadata = merge(
    {
      aws_cluster_name = local.cluster_info.cluster_name
      aws_region       = local.region
      aws_account_id   = data.aws_caller_identity.current.account_id
      aws_vpc_id       = module.vpc.vpc_id
      use_ack          = local.use_ack
    },
    {
      addons_repo_url      = local.gitops_addons_repo_url
      addons_repo_path     = var.gitops_addons_repo_path
      addons_repo_basepath = var.gitops_addons_repo_base_path
      addons_repo_revision = var.gitops_addons_repo_revision
    },
    {
      fleet_repo_url      = local.gitops_fleet_repo_url
      fleet_repo_path     = var.gitops_fleet_repo_path
      fleet_repo_basepath = var.gitops_fleet_repo_base_path
      fleet_repo_revision = var.gitops_fleet_repo_revision
    },
    {
      external_secrets_namespace       = local.external_secrets.namespace
      external_secrets_service_account = local.external_secrets.service_account
    }
  )

  argocd_apps = {
    applicationsets = file("${path.module}/bootstrap/applicationsets.yaml")
  }
  role_arns = []
  # # Generate dynamic access entries for each admin rolelocals {
  admin_access_entries = {
    for role_arn in local.role_arns : role_arn => {
      principal_arn = role_arn
      policy_associations = {
        admins = {
          policy_arn = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"
          access_scope = {
            type = "cluster"
          }
        }
      }
    }
  }


  # Merging dynamic entries with static entries if needed
  access_entries = merge({}, local.admin_access_entries)

  tags = {
    Blueprint  = local.name
    GithubRepo = "github.com/gitops-bridge-dev/gitops-bridge"
  }
}


