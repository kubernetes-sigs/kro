# Contributing Guidelines

Thank you for your interest in contributing to our project. Whether it's a bug report, new feature, correction, or additional
documentation, we greatly value feedback and contributions from our community.

Please read through this document before submitting any issues or pull requests to ensure we have all the necessary
information to effectively respond to your bug report or contribution.

## Reporting Bugs/Feature Requests

We welcome you to use the GitHub issue tracker to report bugs or suggest features.

When filing an issue, please check existing open, or recently closed, issues to make sure somebody else hasn't already
reported the issue. Please try to include as much information as you can. Details like these are incredibly useful:

* A reproducible test case or series of steps
* The version of our code being used
* Any modifications you've made relevant to the bug
* Anything unusual about your environment or deployment


## Contributing via Pull Requests
Contributions via pull requests are much appreciated. Before sending us a pull request, please ensure that:

1. You are working against the latest source on the *main* branch.
2. You check existing open, and recently merged, pull requests to make sure someone else hasn't addressed the problem already.
3. You open an issue to discuss any significant work - we would hate for your time to be wasted.

To send us a pull request, please:

1. Fork the repository.
2. Modify the source; please focus on the specific change you are contributing. If you also reformat all the code, it will be hard for us to focus on your change.
3. Ensure local tests pass.
4. Commit to your fork using clear commit messages.
5. Send us a pull request, answering any default questions in the pull request interface.
6. Pay attention to any automated CI failures reported in the pull request, and stay involved in the conversation.

GitHub provides additional document on [forking a repository](https://help.github.com/articles/fork-a-repo/) and
[creating a pull request](https://help.github.com/articles/creating-a-pull-request/).

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
    make manifests
    kubectl apply -k ./config/crd
    ```

4. Run the kro Controller Locally: Execute the controller with your changes:
    ```bash
    go run ./cmd/controller --log-level 2
    ```
    This will connect to the default Kubernetes context in your local kubeconfig (`~/.kube/config`). Ensure the context is pointing to your local cluster.

### Running the controller inside a [`KinD`][kind] cluster with [`ko`][ko]

[ko]: https://ko.build
[kind]: https://kind.sigs.k8s.io/

1. Create a `KinD` cluster.

   ```sh
   kind create cluster
   ```

2. Create the `kro-system` namespace.

   ```sh
   kubectl create namespace kro-system
   ```

3. Set the `KO_DOCKER_REPO` env var.

   ```sh
   export KO_DOCKER_REPO=kind.local
   ```

   > _Note_, if not using the default kind cluster name, set KIND_CLUSTER_NAME

   ```sh
   export KIND_CLUSTER_NAME=my-other-cluster
   ```
4. Apply the Kro CRDs.

   ```sh
   make manifests
   kubectl apply -f ./helm/crds
   ```

5. Render and apply the local helm chart.

   ```sh
    helm template kro ./helm \
      --namespace kro-system \
      --set image.pullPolicy=Never \
      --set image.ko=true | ko apply -f -
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

## Finding contributions to work on
Looking at the existing issues is a great way to find something to contribute on. As our projects, by default, use the default GitHub issue labels (enhancement/bug/duplicate/help wanted/invalid/question/wontfix), looking at any 'help wanted' issues is a great place to start.


## Code of Conduct

This project has adopted the [CNCF Code of Conduct](https://github.com/cncf/foundation/blob/main/code-of-conduct.md).


## Security 

TODO


## Licensing

See the [LICENSE](LICENSE) file for our project's licensing. We will ask you to confirm the licensing of your contribution.
