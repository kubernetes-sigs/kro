# Ollama Kubernetes Deployment

Resource Graph Definition (RGD) for deploying Ollama with various LLM models. It supports both CPU and GPU deployments with configurable resources and automatic model pulling.

## Overview

The RGD creates the following Kubernetes resources:
- Storage Class (AWS EBS GP3)
- Persistent Volume Claim for model storage
- Model Pull Deployment (prepares the model)
- Serve Deployment (runs the inference service)
- Service (internal networking)
- Ingress (optional, for external access)

## Prerequisites

### Required
- Kubernetes cluster
- `kubectl` configured to access your cluster
- AWS EBS CSI Driver
  ```bash
  helm repo add aws-ebs-csi-driver https://kubernetes-sigs.github.io/aws-ebs-csi-driver
  helm install aws-ebs-csi-driver aws-ebs-csi-driver/aws-ebs-csi-driver
  ```

### Optional (Based on Configuration)
- AWS Load Balancer Controller (required if ingress is enabled)
  ```bash
  helm repo add eks https://aws.github.io/eks-charts
  helm install aws-load-balancer-controller eks/aws-load-balancer-controller
  ```
- NVIDIA GPU Operators (required if GPU is enabled)
  ```bash
  helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
  helm install nvidia-device-plugin nvidia/gpu-operator
  ```

## Configuration

Create an instance YAML file with your desired configuration:

```yaml
apiVersion: kro.run/v1alpha1
kind: OllamaDeployment
metadata:
  name: llm
spec:
  name: llm        
  namespace: default
  values:
    storage: 50Gi        # Storage for model files
    model:
      name: llama3.2 # phi | deepseek - Model name
      size: 1b         # Model size variant
    resources:
      requests:
        cpu: "2"
        memory: "8Gi"
      limits:
        cpu: "4"
        memory: "16Gi"
    gpu:
      enabled: false     # Set to true for GPU acceleration
      count: 1          # deployment limit for number of GPUs to allocate
    ingress:
      enabled: false    # Set to true for external access
      port: 80         # Ingress port
```

## Deployment

1. Apply the RGD:
```bash
kubectl apply -f RGD.yaml
```

2. Create your instance:
```bash
kubectl apply -f instance.yaml
```

## Usage

### Internal Access
Access the model within the cluster using:
```bash
curl http://<name>.<namespace>.svc.cluster.local/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{ 
    "model": "llama3.2:1b",
    "prompt": "Hello, who are you?"
  }'
```

### External Access (if ingress enabled)
The service will be available at:
```bash
curl http://<ALB-ADDRESS>/<name>/v1/completions \
  -H 'Content-Type: application/json' \
  -d '{ 
    "model": "llama3.2:1b",
    "prompt": "Hello, who are you?"
  }'
```

## Resource Requirements

### Minimum Requirements
- CPU deployment: 2 CPU cores, 8GB RAM
- GPU deployment: 2 CPU cores, 8GB RAM, 1 NVIDIA GPU
- Storage: 50GB (adjust based on model size)

### Scaling Considerations
- Increase storage for larger models
- Adjust CPU/Memory based on load
- Add GPUs for better inference performance
