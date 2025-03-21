vpc_name                        = "hub-cluster"
kubernetes_version              = "1.32"
cluster_name                    = "hub-cluster"
tenant                          = "tenant1"
region                          = "eu-west-2"

#Create the GitHub repo `gitops-fleet-management` in your organization; Use your organization as the git_org_name below
git_org_name                    = "xxxxxxxxx"

gitops_addons_repo_name         = "gitops-fleet-management"
gitops_addons_repo_base_path    = "addons/"
gitops_addons_repo_path         = "bootstrap"
gitops_addons_repo_revision     = "main"

gitops_fleet_repo_name           = "gitops-fleet-management"
gitops_fleet_repo_base_path      = "fleet/"
gitops_fleet_repo_path           = "bootstrap"
gitops_fleet_repo_revision       = "main"

gitops_platform_repo_name       = "gitops-fleet-management"
gitops_platform_repo_base_path  = "platform/"
gitops_platform_repo_path       = "bootstrap"
gitops_platform_repo_revision   = "main"

gitops_workload_repo_name       = "gitops-fleet-management"
gitops_workload_repo_base_path  = "apps/"
gitops_workload_repo_path       = ""
gitops_workload_repo_revision   = "main"

use_ack                         = true
enable_automode                 = true
addons = {
  enable_metrics_server               = true
  enable_kyverno                      = true    
  enable_kyverno_policies             = true
  enable_kyverno_policy_reporter      = true
  enable_argocd                       = true
  enable_cni_metrics_helper           = false
  enable_kube_state_metrics           = true
  enable_cert_manager                 = false
  enable_external_dns                 = false
  enable_external_secrets             = true
  enable_ack_iam                      = true
  enable_ack_eks                      = true
  enable_ack_ec2                      = true
  enable_ack_efs                      = true
  enable_kro                          = true
  enable_kro_eks_rgs                  = true
  enable_mutli_acct                   = true
}

# Insert your own AWS Accounts here (cluster1, cluster2)
account_ids = "586794472760 825765380480"
