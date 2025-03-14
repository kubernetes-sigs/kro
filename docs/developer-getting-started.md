# Developer Getting Started

## Getting and running local tools

Tools in this project are generally managed as [tool dependencies](https://tip.golang.org/doc/modules/managing-dependencies#tools),
and where possible hooked into the standard `go` commands. Here's how you accomplish some common tasks in this repository:

### Build the project

Run `go vet ./...` to ensure that the project can be built, and `go build -o kro-controller cmd/controller/main.go` to produce a binary.

### Run tests

`go test ./...` runs all tests in the project. To skip the integration tests (they can be a bit on the slow side), run `go test ./pkg/...`.

### Lint

We have also set up `golangci-lint`, so you can run `go tool golangci-lint run` to see any linter warnings.

### Generate code and manifests

Everything should be hooked up through `go generate ./...`, so that's all you're going to need.

You might want to do `go generate ./api/...` while iterating on the **kro** APIs, to avoid having to wait for the license and attribution generation each time.

### Build a local Docker iamge

`go tool ko build --local -B github.com/kro-run/kro/cmd/controller --push=false --sbom=none` will get you a Docker iamge you can play around with. Its name is printed at the end of the command output, and is the only thing written to stdout, so you can use its output as input to other commands (e.g. `docker run`).

## Setting Up a Local Development Environment

By following the steps for [externally running a controller](#running-the-controller-external-to-the-cluster) or
[running the controller inside a `KinD` cluster](#running-the-controller-inside-a-kind-cluster-with-ko), you can set up
a local environment to test your contributions before submitting a pull request.

### Create a Local Development Cluster

We recommend using [kind] to create and manage local Kubernetes clusters. After installing it, creating a cluster is as easy as

```sh
export KIND_CLUSTER_NAME=kro
kind create cluster
```

This starts up a local control plane and modifies your kubeconfig to point to it. If you already have a cluster, you can get
its config and reconnect e.g. by doing something like

```sh
kind get kubeconfig --name kro > /tmp/kro.kubeconfig
export KUBECONFIG=/tmp/kro.kubeconfig
```

In order to deploy **kro** into this cluster, you also need to ensure the namespace `kro-system` exists:

```sh
kubectl create ns kro-system || true
```

### Running the controller external to the cluster

1. Install the Custom Resource Definitions (CRDs): Apply the latest CRDs to your cluster:

   ```bash
   go generate ./api/...
   kubectl apply -f ./helm/templates/crds
   ```

2. Run the kro Controller Locally: Execute the controller with your changes:

   ```bash
   go run ./cmd/controller --log-level 2
   ```

### Running the controller inside a [kind] cluster with [ko]

1. Build and install KRO components

   ```bash
   # Re-generate the KRO CRDs
   go generate ./api/...

   # Render the Helm chart and apply using ko
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

2. Create an instance of the new `NoOp` kind.

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

[kind]: https://kind.sigs.k8s.io
[ko]: https://ko.build
