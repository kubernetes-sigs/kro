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

################################################################################
# EBS CSI EKS Access
################################################################################
# module "aws_ebs_csi_pod_identity" {
#   source  = "terraform-aws-modules/eks-pod-identity/aws"
#   version = "~> 1.4.0"

#   name = "aws-ebs-csi"

#   attach_aws_ebs_csi_policy = true
#   aws_ebs_csi_kms_arns      = ["arn:aws:kms:*:*:key/*"]

#   # Pod Identity Associations
#   associations = {
#     addon = {
#       cluster_name    = local.cluster_info.cluster_name
#       namespace       = "kube-system"
#       service_account = "ebs-csi-controller-sa"
#     }
#   }

#   tags = local.tags
# }

################################################################################
# AWS ALB Ingress Controller EKS Access
################################################################################
# module "aws_lb_controller_pod_identity" {
#   count   = local.aws_addons.enable_aws_load_balancer_controller || local.enable_automode ? 1 : 0
#   source  = "terraform-aws-modules/eks-pod-identity/aws"
#   version = "~> 1.4.0"

#   name = "aws-lbc"

#   attach_aws_lb_controller_policy = true


#   # Pod Identity Associations
#   associations = {
#     addon = {
#       cluster_name    = local.cluster_info.cluster_name
#       namespace       = local.aws_load_balancer_controller.namespace
#       service_account = local.aws_load_balancer_controller.service_account
#     }
#   }

#   tags = local.tags
# }

################################################################################
# Karpenter EKS Access
################################################################################

module "argocd_hub_pod_identity" {
  source  = "terraform-aws-modules/eks-pod-identity/aws"
  version = "~> 1.4.0"

  name      = "argocd-hub-mgmt"
  use_name_prefix = false

  attach_custom_policy = true
  policy_statements = [
    {
      sid       = "ArgoCD"
      actions   = ["sts:AssumeRole", "sts:TagSession"]
      resources = ["*"]
    }
  ]

  # Pod Identity Associations
  association_defaults = {
    namespace = "argocd"
  }
  associations = {
    controller = {
      cluster_name    = local.cluster_info.cluster_name
      service_account = "argocd-application-controller"
    }
    server = {
      cluster_name    = local.cluster_info.cluster_name
      service_account = "argocd-server"
    }
    repo-server = {
      cluster_name    = local.cluster_info.cluster_name
      service_account = "argocd-repo-server"
    }
  }

  tags = local.tags
}


# Define variables for the policy URLs
variable "policy_arn_urls" {
  type    = map(string)
  default = {
    iam = "https://raw.githubusercontent.com/aws-controllers-k8s/iam-controller/main/config/iam/recommended-policy-arn"
    ec2 = "https://raw.githubusercontent.com/aws-controllers-k8s/ec2-controller/main/config/iam/recommended-policy-arn"
    eks = "https://raw.githubusercontent.com/aws-controllers-k8s/eks-controller/main/config/iam/recommended-policy-arn"
  }
}

variable "inline_policy_urls" {
  type    = map(string)
  default = {
    iam = "https://raw.githubusercontent.com/aws-controllers-k8s/iam-controller/main/config/iam/recommended-inline-policy"
    ec2 = "https://raw.githubusercontent.com/aws-controllers-k8s/ec2-controller/main/config/iam/recommended-inline-policy"
    eks = "https://raw.githubusercontent.com/aws-controllers-k8s/eks-controller/main/config/iam/recommended-inline-policy"
  }
}

# Fetch the recommended policy ARNs
data "http" "policy_arn" {
  for_each = var.policy_arn_urls
  url      = each.value
}

# Fetch the recommended inline policies
data "http" "inline_policy" {
  for_each = var.inline_policy_urls
  url      = each.value
}

# Create IAM roles for ACK controllers
resource "aws_iam_role" "ack_controller" {
  for_each = toset(["iam", "ec2", "eks"])
  name        = "ack-${each.key}-controller-role-mgmt"
  
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Sid = "AllowEksAuthToAssumeRoleForPodIdentity"
        Effect = "Allow"
        Principal = {
          Service = "pods.eks.amazonaws.com"
        }
        Action = ["sts:AssumeRole", "sts:TagSession"]
      }
    ]
  })
  description = "IRSA role for ACK ${each.key} controller deployment on EKS cluster using Helm charts"
  tags        = local.tags
}

# First, create a local variable to determine valid policies
locals {
  valid_policies = {
    for k, v in data.http.policy_arn : k => v.status_code == 200 ? trimspace(v.body) : null
  }
}

# Then modify your policy attachment to only create when there's a valid ARN
resource "aws_iam_role_policy_attachment" "ack_controller_policy_attachment" {
  for_each = {
    for k, v in local.valid_policies : k => v
    if v != null && can(regex("^arn:aws", v))
  }

  role       = aws_iam_role.ack_controller[each.key].name
  policy_arn = each.value
}

resource "aws_iam_role_policy" "ack_controller_inline_policy" {
  for_each = toset(["iam", "ec2", "eks"])

  role   = aws_iam_role.ack_controller[each.key].name
  policy = can(jsondecode(data.http.inline_policy[each.key].body)) ? data.http.inline_policy[each.key].body : jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "${each.key}:*"
        ]
        Resource = "*"
      }
    ]
  })
}

data "aws_iam_policy_document" "ack_controller_cross_account_policy" {
  for_each = toset(["iam", "ec2", "eks"])

  statement {
    sid    = "AllowCrossAccountAccess"
    effect = "Allow"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    resources = [
      for account in split(" ", var.account_ids) : "arn:aws:iam::${account}:role/eks-cluster-mgmt-${each.key}"
    ]
  }
}

resource "aws_iam_role_policy" "ack_controller_cross_account_policy" {
  for_each = toset(["iam", "ec2", "eks"])

  role   = aws_iam_role.ack_controller[each.key].name
  policy = data.aws_iam_policy_document.ack_controller_cross_account_policy[each.key].json
}

resource "aws_eks_pod_identity_association" "ack_controller" {
  for_each = toset(["iam", "ec2", "eks"])

  cluster_name    = local.cluster_info.cluster_name
  namespace       = "ack-system"
  service_account = "ack-${each.key}-controller"
  role_arn        = aws_iam_role.ack_controller[each.key].arn
}