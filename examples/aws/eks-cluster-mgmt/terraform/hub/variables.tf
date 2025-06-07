variable "vpc_name" {
  description = "VPC name to be used by pipelines for data"
  type        = string
}

variable "kubernetes_version" {
  description = "Kubernetes version"
  type        = string
  default     = "1.31"
}

variable "github_app_credentilas_secret" {
  description = "The name of the Secret storing github app credentials"
  type        = string
  default     = ""
}

variable "kms_key_admin_roles" {
  description = "list of role ARNs to add to the KMS policy"
  type        = list(string)
  default     = []
}

variable "addons" {
  description = "Kubernetes addons"
  type        = any
  default     = {
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
    enable_multi_acct                   = true
  }
}

variable "manifests" {
  description = "Kubernetes manifests"
  type        = any
  default     = {}
}

variable "enable_addon_selector" {
  description = "select addons using cluster selector"
  type        = bool
  default     = false
}

variable "route53_zone_name" {
  description = "The route53 zone for external dns"
  default     = ""
}
# Github Repos Variables

variable "git_org_name" {
  description = "The name of Github organisation"
  default     = "kro-run"
}

variable "gitops_addons_repo_name" {
  description = "The name of git repo"
  default     = "kro"
}

variable "gitops_addons_repo_path" {
  description = "The path of addons bootstraps in the repo"
  default     = "bootstrap"
}

variable "gitops_addons_repo_base_path" {
  description = "The base path of addons in the repon"
  default     = "examples/aws/eks-cluster-mgmt/addons/"
}

variable "gitops_addons_repo_revision" {
  description = "The name of branch or tag"
  default     = "main"
}
# Fleet
variable "gitops_fleet_repo_name" {
  description = "The name of Git repo"
  default     = "kro"
}

variable "gitops_fleet_repo_path" {
  description = "The path of fleet bootstraps in the repo"
  default     = "bootstrap"
}

variable "gitops_fleet_repo_base_path" {
  description = "The base path of fleet in the repon"
  default     = "examples/aws/eks-cluster-mgmt/fleet/"
}

variable "gitops_fleet_repo_revision" {
  description = "The name of branch or tag"
  default     = "main"
}

# workload
variable "gitops_workload_repo_name" {
  description = "The name of Git repo"
  default     = "kro"
}

variable "gitops_workload_repo_path" {
  description = "The path of workload bootstraps in the repo"
  default     = "examples/aws/eks-cluster-mgmt/apps/"
}

variable "gitops_workload_repo_base_path" {
  description = "The base path of workloads in the repo"
  default     = ""
}

variable "gitops_workload_repo_revision" {
  description = "The name of branch or tag"
  default     = "main"
}

# Platform
variable "gitops_platform_repo_name" {
  description = "The name of Git repo"
  default     = "kro"
}

variable "gitops_platform_repo_path" {
  description = "The path of platform bootstraps in the repo"
  default     = "bootstrap"
}

variable "gitops_platform_repo_base_path" {
  description = "The base path of platform in the repo"
  default     = "examples/aws/eks-cluster-mgmt/platform/"
}

variable "gitops_platform_repo_revision" {
  description = "The name of branch or tag"
  default     = "main"
}


variable "ackCreate" {
  description = "Creating PodIdentity and addons relevant resources with ACK"
  default     = false
}

variable "enable_efs" {
  description = "Enabling EFS file system"
  type        = bool
  default     = false
}

variable "enable_automode" {
  description = "Enabling Automode Cluster"
  type        = bool
  default     = true
}

variable "cluster_name" {
  description = "Name of the cluster"
  type        = string
  default     = "hub-cluster"
}

variable "use_ack" {
  description = "Defining to use ack or terraform for pod identity if this is true then we will use this label to deploy resouces with ack"
  type        = bool
  default     = true
}

variable "environment" {
  description = "Name of the environment for the Hub Cluster"
  type        = string
  default     = "control-plane"
}

variable "tenant" {
  description = "Name of the tenant for the Hub Cluster"
  type        = string
  default     = "control-plane"
}

variable "account_ids" {
  description = "List of aws accounts ACK will need to connect to"
  type        = string
  default     = ""
}