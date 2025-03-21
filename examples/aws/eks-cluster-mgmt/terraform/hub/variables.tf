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
  default     = {}
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
  default     = ""
}

variable "gitops_addons_repo_name" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_addons_repo_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_addons_repo_base_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_addons_repo_revision" {
  description = "The name of Github organisation"
  default     = ""
}
# Fleet
variable "gitops_fleet_repo_name" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_fleet_repo_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_fleet_repo_base_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_fleet_repo_revision" {
  description = "The name of Github organisation"
  default     = ""
}

# workload
variable "gitops_workload_repo_name" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_workload_repo_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_workload_repo_base_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_workload_repo_revision" {
  description = "The name of Github organisation"
  default     = ""
}

# Platform
variable "gitops_platform_repo_name" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_platform_repo_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_platform_repo_base_path" {
  description = "The name of Github organisation"
  default     = ""
}

variable "gitops_platform_repo_revision" {
  description = "The name of Github organisation"
  default     = ""
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
  default     = false
}

variable "cluster_name" {
  description = "Name of the cluster"
  type        = string
  default     = "hub-cluster"
}

variable "use_ack" {
  description = "Defining to use ack or terraform for pod identity if this is true then we will use this label to deploy resouces with ack"
  type        = bool
  default     = false
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

variable "region" {
  description = "Name AWS region to deploy to"
  type        = string
  default     = "eu-west-2"
}

variable "account_ids" {
  description = "List of aws accounts ACK will need to connect to"
  type        = string
  default     = ""
}