# Developer Getting Started (macOS + kind)

**Save this file as:** `docs/developer-getting-started.md`

This guide helps you run **KRO locally** on a Mac using a local Kubernetes cluster (kind). You do **not** need VirtualBox.

---

## Prerequisites

### Required

* **Docker Desktop for Mac** (must be running)
* **kubectl**
* **Helm**
* **kind**
* **git**

### Recommended (for code contributions)

* **Go** (version required by the repo’s `go.mod`)
* **make** (usually already available on macOS)

### Install tools (Homebrew)

```bash
brew install kubectl helm kind git go
```

Confirm:

```bash
docker version
kubectl version --client
helm version
kind version
go version
```

---

## Get the source code

> If you only need to run KRO locally (not contribute code), you can skip cloning the repo.

Fork the repo on GitHub, then:

```bash
git clone https://github.com/<your-username>/kro.git
cd kro

git remote add upstream https://github.com/kro-run/kro.git
git fetch upstream
```

Create a working branch:

```bash
git checkout -b docs/dev-getting-started
```

---

## Create a local Kubernetes cluster (kind)

Create a cluster named `kro`:

```bash
kind create cluster --name kro
kubectl config use-context kind-kro
kubectl get nodes
```

Expected:

* One node: `kro-control-plane`
* `STATUS` should be `Ready`

---

## Install KRO (Helm)

Install the controller into `kro-system`:

```bash
helm install kro oci://registry.k8s.io/kro/charts/kro \
  --namespace kro-system \
  --create-namespace
```

Verify the controller is running:

```bash
kubectl get pods -n kro-system
```

Expected:

* A pod like `kro-...` in `Running` state

---

## Quick Start (Create an API using an RGD)

KRO uses a `ResourceGraphDefinition` (RGD) to define a new API that expands into one or more Kubernetes resources.

### 1) Create an RGD that defines an `Application` kind

```bash
kubectl apply -f - <<'EOF'
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-application
spec:
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      name: string
      image: string | default="nginx"
      replicas: integer | default=1
    status:
      availableReplicas: ${deployment.status.availableReplicas}
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: ${schema.spec.name}
                  image: ${schema.spec.image}
                  ports:
                    - containerPort: 80
    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}-svc
        spec:
          selector: ${deployment.spec.selector.matchLabels}
          ports:
            - protocol: TCP
              port: 80
              targetPort: 80
EOF
```

Check RGD status:

```bash
kubectl get rgd
```

Expected:

* `my-application` shows `STATE: Active`

### 2) Create an instance of the new `Application` kind

```bash
kubectl apply -f - <<'EOF'
apiVersion: kro.run/v1alpha1
kind: Application
metadata:
  name: my-app-instance
spec:
  name: my-app
  image: nginx
  replicas: 1
EOF
```

Verify:

```bash
kubectl get applications
kubectl get deploy,svc
```

Expected:

* `Application` is `ACTIVE` with `Ready=True`
* A Deployment named `my-app`
* A Service named `my-app-svc`

---

## Optional: Test the service locally

Port-forward the service:

```bash
kubectl port-forward svc/my-app-svc 8080:80
```

Then open:

* `http://localhost:8080`

Stop with `Ctrl+C`.

---