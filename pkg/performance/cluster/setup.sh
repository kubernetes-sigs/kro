#!/bin/bash
set -e

# Create a Kind cluster for testing
echo "Setting up a Kind cluster for performance testing..."

# Create kind config file if it doesn't exist
if [ ! -f "kind-config.yaml" ]; then
    cat > kind-config.yaml << 'KINDEOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: InitConfiguration
    nodeRegistration:
      kubeletExtraArgs:
        node-labels: "ingress-ready=true"
  extraPortMappings:
  - containerPort: 80
    hostPort: 80
    protocol: TCP
  - containerPort: 443
    hostPort: 443
    protocol: TCP
- role: worker
- role: worker
KINDEOF
    echo "Created kind-config.yaml for 3-node cluster (1 control plane, 2 workers)"
fi

# Check if Kind is installed
if ! command -v kind &> /dev/null; then
    echo "Kind not found, installing..."
    curl -Lo ./kind https://kind.sigs.k8s.io/dl/latest/kind-linux-amd64
    chmod +x ./kind
    sudo mv ./kind /usr/local/bin/kind
fi

# Create cluster
echo "Creating Kind cluster 'kro-perf'..."
kind create cluster --name kro-perf --config kind-config.yaml

# Install Prometheus Operator for monitoring (optional)
echo "Installing Prometheus for monitoring..."
kubectl create namespace monitoring || true
kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/master/bundle.yaml -n monitoring

# Install metrics server for resource monitoring
echo "Installing metrics server..."
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml

# Deploy KRO (assuming kro.yaml is in the deploy directory)
echo "Deploying KRO..."
kubectl create namespace kro-system || true
kubectl apply -f ../deploy/kro.yaml -n kro-system

echo "Cluster setup complete. Use 'kind delete cluster --name kro-perf' to clean up when finished."
echo "To access the cluster, use: export KUBECONFIG=$(kind get kubeconfig --name kro-perf)"

# Print cluster info
kubectl cluster-info --context kind-kro-perf
kubectl get nodes
kubectl get pods -A
