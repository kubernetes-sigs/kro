# Create ArgoCD namespace
resource "kubernetes_namespace_v1" "argocd" {
  metadata {
    name = local.argocd_namespace
  }
}

locals {
  cluster_name = module.eks.cluster_name
  argocd_labels = merge({
    cluster_name                     = local.cluster_name
    environment                      = local.environment
    enable_argocd                    = true
    "argocd.argoproj.io/secret-type" = "cluster"
    },
    try(local.addons, {})
  )
  argocd_annotations = merge(
    {
      cluster_name = local.cluster_name
      environment  = local.environment
    },
    try(local.addons_metadata, {})
  )
}

locals {
  config = <<-EOT
    {
      "tlsClientConfig": {
        "insecure": false
      }
    }
  EOT
  argocd = {
    apiVersion = "v1"
    kind       = "Secret"
    metadata = {
      name        = module.eks.cluster_name
      namespace   = local.argocd_namespace
      annotations = local.argocd_annotations
      labels      = local.argocd_labels
    }
    stringData = {
      name   = module.eks.cluster_name
      server = module.eks.cluster_arn
      project = "default"
    }
  }
}
resource "kubernetes_secret_v1" "cluster" {
  metadata {
    name        = local.argocd.metadata.name
    namespace   = local.argocd.metadata.namespace
    annotations = local.argocd.metadata.annotations
    labels      = local.argocd.metadata.labels
  }
  data = local.argocd.stringData
}