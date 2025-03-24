vpc_name                        = "hub-cluster"
kubernetes_version              = "1.32"
cluster_name                    = "hub-cluster"
tenant                          = "tenant1"

git_org_name                    = "XXXXXXXX" # update this if you want to customize the gitops configurations

gitops_addons_repo_name         = "eks-cluster-mgmt"
gitops_addons_repo_base_path    = "addons/"
gitops_addons_repo_path         = "bootstrap"
gitops_addons_repo_revision     = "main"

gitops_fleet_repo_name           = "eks-cluster-mgmt"
gitops_fleet_repo_base_path      = "fleet/"
gitops_fleet_repo_path           = "bootstrap"
gitops_fleet_repo_revision       = "main"

gitops_platform_repo_name       = "eks-cluster-mgmt"
gitops_platform_repo_base_path  = "platform/"
gitops_platform_repo_path       = "bootstrap"
gitops_platform_repo_revision   = "main"

gitops_workload_repo_name       = "eks-cluster-mgmt"
gitops_workload_repo_base_path  = "apps/"
gitops_workload_repo_path       = ""
gitops_workload_repo_revision   = "main"


# AWS Accounts used for demo purposes (cluster1 cluster2)
account_ids = "012345678910 123456789101" # update this with your spoke aws accounts ids
