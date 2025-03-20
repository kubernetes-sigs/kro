################################################################################
# GitOps Bridge: Private ssh keys for git
################################################################################
resource "kubernetes_namespace" "argocd" {
  depends_on = [local.cluster_info]

  metadata {
    name = local.argocd_namespace
  }
}
resource "kubernetes_secret" "git_secrets" {
  depends_on = [kubernetes_namespace.argocd]
  for_each = {
    # git-addons = {
    #   type                    = "git"
    #   url                     = "https://github.com/eks-fleet-management/gitops-addons-private.git"
    #   githubAppID             = local.git_data["github_app_id"]
    #   githubAppInstallationID = local.git_data["github_app_installation_id"]
    #   githubAppPrivateKey     = base64decode(local.git_data["github_private_key"])
    # }
    # git-fleet = {
    #   type                    = "git"
    #   url                     = "https://github.com/eks-fleet-management/gitops-fleet.git"
    #   githubAppID             = local.git_data["github_app_id"]
    #   githubAppInstallationID = local.git_data["github_app_installation_id"]
    #   githubAppPrivateKey     = base64decode(local.git_data["github_private_key"])
    # }
    # git-resources = {
    #   type                    = "git"
    #   url                     = "https://github.com/eks-fleet-management/gitops-resources.git"
    #   githubAppID             = local.git_data["github_app_id"]
    #   githubAppInstallationID = local.git_data["github_app_installation_id"]
    #   githubAppPrivateKey     = base64decode(local.git_data["github_private_key"])
    # }
  }
  metadata {
    name      = each.key
    namespace = kubernetes_namespace.argocd.metadata[0].name
    labels = {
      "argocd.argoproj.io/secret-type" = "repository"
    }
  }
  data = each.value
}

# Creating parameter for argocd hub role for the spoke clusters to read
resource "aws_ssm_parameter" "argocd_hub_role" {
  name  = "/${local.name}/argocd-hub-role"
  type  = "String"
  value = module.argocd_hub_pod_identity.iam_role_arn
}
################################################################################
# GitOps Bridge: Bootstrap
################################################################################
module "gitops_bridge_bootstrap" {
  source  = "gitops-bridge-dev/gitops-bridge/helm"
  version = "0.1.0"
  cluster = {
    cluster_name = module.eks.cluster_name
    environment  = local.environment
    metadata     = local.addons_metadata
    addons       = local.addons
  }

  apps = local.argocd_apps
  argocd = {
    name             = "argocd"
    namespace        = local.argocd_namespace
    chart_version    = "7.7.8"
    values           = [file("${path.module}/argocd-initial-values.yaml")]
    timeout          = 600
    create_namespace = false
  }
  depends_on = [kubernetes_secret.git_secrets]
}
