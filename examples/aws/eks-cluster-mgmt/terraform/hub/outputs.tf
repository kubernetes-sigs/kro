# Output the ACK controller role ARN
output "ack_controller_role_arn" {
  description = "ARN of the IAM role for ACK controller"
  value       = aws_iam_role.ack_controller.arn
}

# Output the kro controller role ARN
output "kro_controller_role_arn" {
  description = "ARN of the IAM role for kro controller"
  value       = aws_iam_role.kro_controller.arn
}

# Output the argocd controller role ARN
output "argocd_controller_role_arn" {
  description = "ARN of the IAM role for argocd controller"
  value       = aws_iam_role.argocd_controller.arn
}

# Output cluster name
output "cluster_name" {
  description = "Name of the EKS cluster"
  value       = module.eks.cluster_name
}