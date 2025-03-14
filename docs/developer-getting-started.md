
# Developer Getting Started

## Getting and running local tools

Tools in this project are generally managed as [tool dependencies](https://tip.golang.org/doc/modules/managing-dependencies#tools),
and where possible hooked into the standard `go` commands. Here's how you accomplish some common tasks in this repository:

* **Build the project**: `go vet ./...` to ensure that the project can be built, and `go build -o kro-controller cmd/controller/main.go` to produce a binary.
* **Run tests**: `go test ./...` runs all tests in the project. To skip the integration tests (they can be a bit on the slow side), run `go test ./pkg/...`.
* **Lint**: We have also set up `golangci-lint`, so you can run `go tool golangci-lint run` to see any linter warnings.
* **Generate code and manifests**: Everything should be hooked up through `go generate ./...`

## Setting Up a Local Development Environment

By following the steps for [externally running a controller](#running-the-controller-external-to-the-cluster) or 
[running the controller inside a `KinD` cluster](#running-the-controller-inside-a-kind-cluster-with-ko), you can set up 
a local environment to test your contributions before submitting a pull request.

### Running the controller external to the cluster

To test and run the project with your local changes, follow these steps to set up a development environment:

1. Install Dependencies: Ensure you have the necessary dependencies installed, including:
    - [Go](https://golang.org/doc/install) (version specified in `go.mod`).
    - [kubectl](https://kubernetes.io/docs/tasks/tools/#kubectl) for interacting with Kubernetes clusters.
    - A local Kubernetes cluster such as [kind](https://kind.sigs.k8s.io/).

2. Create a Local Kubernetes Cluster: If you don't already have a cluster, create one with your preferred tool. For example, with `kind`:
    ```bash
    kind create cluster
    ```

3. Install the Custom Resource Definitions (CRDs): Apply the latest CRDs to your cluster:
    ```bash
    go generate ./api/...
    kubectl apply -f ./helm/templates/crds
    ```

4. Run the kro Controller Locally: Execute the controller with your changes:
    ```bash
    go run ./cmd/controller --log-level 2
    ```
    This will connect to the default Kubernetes context in your local kubeconfig (`~/.kube/config`). Ensure the context is pointing to your local cluster.

### Running the controller inside a [`KinD`][kind] cluster with [`ko`][ko]

[ko]: https://ko.build
[kind]: https://kind.sigs.k8s.io/

For iterating on an existing cluster, follow the instructions below.

1. Prepare the cluster
```sh
export KIND_CLUSTER_NAME=kro-local-dev
# Create a kind cluster if needed
# If you skip this, remember to set your kubecontext to point to your existing cluster,
# as kind will do this automatically when creating a new one!
kind create cluster

## Create the kro-system namespace
kubectl create namespace kro-system || true
```

2. Build and install KRO components

```sh
## install the KRO CRDs
go generate ./api/...
kubectl apply -f ./helm/template/crds

# render the helm chart and apply using ko
export KO_DOCKER_REPO=kind.local
helm template kro ./helm \
  --namespace kro-system \
  --set image.pullPolicy=Never \
  --set image.ko=true | go tool ko apply -f -
```

### Dev Environment Hello World

1. Create a `NoOp` ResourceGraph using the `ResourceGraphDefinition`.

   ```sh
   kubectl apply -f - <<EOF
   apiVersion: kro.run/v1alpha1
   kind: ResourceGraphDefinition
   metadata:
     name: noop
   spec:
     schema:
       apiVersion: v1alpha1
       kind: NoOp
       spec: {}
       status: {}
     resources: []
   EOF
   ```

   Inspect that the `ResourceGraphDefinition` was created, and also the newly created CRD `NoOp`.

   ```sh
   kubectl get ResourceGraphDefinition noop
   kubectl get crds | grep noops
   ```

3. Create an instance of the new `NoOp` kind.

   ```sh
   kubectl apply -f - <<EOF
   apiVersion: kro.run/v1alpha1
   kind: NoOp
   metadata:
     name: demo
   EOF
   ```

   And inspect the new instance,

   ```shell
   kubectl get noops -oyaml
   ```