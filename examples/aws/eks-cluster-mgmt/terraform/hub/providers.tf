
provider "helm" {
  kubernetes {
    host                   = local.cluster_info.cluster_endpoint
    cluster_ca_certificate = base64decode(local.cluster_info.cluster_certificate_authority_data)

    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      # This requires the awscli to be installed locally where Terraform is executed
      args = [
        "eks",
        "get-token",
        "--cluster-name", local.cluster_info.cluster_name,
        "--region", local.region
      ]
    }
  }
}

provider "kubernetes" {
  host                   = local.cluster_info.cluster_endpoint
  cluster_ca_certificate = base64decode(local.cluster_info.cluster_certificate_authority_data)
  # insecure = true
  exec {
    api_version = "client.authentication.k8s.io/v1beta1"
    command     = "aws"
    # This requires the awscli to be installed locally where Terraform is executed
    args = [
      "eks",
      "get-token",
      "--cluster-name", local.cluster_info.cluster_name,
      "--region", local.region
    ]
  }
}

provider "aws" {
}
