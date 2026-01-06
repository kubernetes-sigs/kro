# IAM role for ACK controllers with assume role capability
resource "aws_iam_role" "ack_controller" {
  name = "${local.name}-ack-controller"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = {
          Service = "capabilities.eks.amazonaws.com"
        }
        Action = [
          "sts:AssumeRole",
          "sts:TagSession"
        ]
      }
    ]
  })

  tags = local.tags
}

# IAM policy allowing the role to assume any role
resource "aws_iam_policy" "ack_assume_role" {
  name        = "${local.name}-ack-assume-role"
  description = "Policy allowing ACK controller to assume any role"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = "sts:AssumeRole"
        Resource = "*"
      }
    ]
  })

  tags = local.tags
}

# Attach the assume role policy to the ACK controller role
resource "aws_iam_role_policy_attachment" "ack_assume_role" {
  role       = aws_iam_role.ack_controller.name
  policy_arn = aws_iam_policy.ack_assume_role.arn
}

# Grant ACK controller role admin access to EKS cluster
resource "aws_eks_access_entry" "ack_controller" {
  cluster_name      = module.eks.cluster_name
  principal_arn     = aws_iam_role.ack_controller.arn
  type              = "STANDARD"
}

resource "aws_eks_access_policy_association" "ack_controller_admin" {
  cluster_name  = module.eks.cluster_name
  principal_arn = aws_iam_role.ack_controller.arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.ack_controller]
}

# IAM role for kro capability
resource "aws_iam_role" "kro_controller" {
  name = "${local.name}-kro-controller"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = {
          Service = "capabilities.eks.amazonaws.com"
        }
        Action = [
          "sts:AssumeRole",
          "sts:TagSession"
        ]
      }
    ]
  })

  tags = local.tags
}

# Grant kro controller role admin access to EKS cluster
resource "aws_eks_access_entry" "kro_controller" {
  cluster_name  = module.eks.cluster_name
  principal_arn = aws_iam_role.kro_controller.arn
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "kro_controller_admin" {
  cluster_name  = module.eks.cluster_name
  principal_arn = aws_iam_role.kro_controller.arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.kro_controller]
}

# IAM role for argocd capability
resource "aws_iam_role" "argocd_controller" {
  name = "${local.name}-argocd-controller"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = {
          Service = "capabilities.eks.amazonaws.com"
        }
        Action = [
          "sts:AssumeRole",
          "sts:TagSession"
        ]
      }
    ]
  })

  tags = local.tags
}

# Grant argocd controller role admin access to EKS cluster
resource "aws_eks_access_entry" "argocd_controller" {
  cluster_name  = module.eks.cluster_name
  principal_arn = aws_iam_role.argocd_controller.arn
  type          = "STANDARD"
}

resource "aws_eks_access_policy_association" "argocd_controller_admin" {
  cluster_name  = module.eks.cluster_name
  principal_arn = aws_iam_role.argocd_controller.arn
  policy_arn    = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy"

  access_scope {
    type = "cluster"
  }

  depends_on = [aws_eks_access_entry.argocd_controller]
}