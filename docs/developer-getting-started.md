
# Developer Getting Started

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
    make install
    ```

4. Run the kro Controller Locally: Execute the controller with your changes:
    ```bash
    go run ./cmd/controller --zap-log-level=info
    ```
    This will connect to the default Kubernetes context in your local kubeconfig (`~/.kube/config`). Ensure the context is pointing to your local cluster.

### Running the controller inside a [`KinD`][kind] cluster with [`ko`][ko]

[ko]: https://ko.build
[kind]: https://kind.sigs.k8s.io/

A helper Makefile target is used to (re)create a kind cluster, install the CRDs, build the container image and helm install the controller manifests in the kind cluster. 

> _Note_: This target re-creates the kind cluster from scratch and should be used as a starting point or when you want to start over fresh.

```sh
KIND_CLUSTER_NAME=kro make deploy-kind
```

For iterating on an existing cluster, follow the instructions below.

1. Prepare the cluster
```sh
export KIND_CLUSTER_NAME=my-other-cluster
# Create a kind cluster if needed
kind create cluster

## Create the kro-system namespace
kubectl create namespace kro-system || true

```

2. Build and install KRO components

```sh
## install the KRO CRDs
make install

# render the helm chart and apply using ko
export KO_DOCKER_REPO=kind.local
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

## Testing

Run tests with `make test WHAT=unit` or `make test WHAT=integration`.

You can pass additional test flags after `--`:

```bash
# Run specific integration tests by pattern
make test WHAT=integration -- --focus 'GraphRevision Integration'

# Run specific unit test
make test WHAT=unit -- -v -run TestBuilder
```

> [!TIP]
> **Focused Package Testing:**
> Instead of running the full suite, target a specific package while working on a feature:
> ```bash
> # Test only the graph compiler
> go test -v ./pkg/graphengine/compiler/...
>
> # Test only condition set logic
> go test -v ./pkg/apis/...
>
> # Run benchmarks for the executor
> go test -bench=. ./pkg/graphengine/executor/...
> ```

> [!NOTE]
> **Code Quality & Linting:**
> Always run these before opening a pull request to catch formatting or static analysis issues early:
> ```bash
> make lint
> make vet
> ```

---

## Codebase Architecture Overview

This section describes every package inside `pkg/` and the role of its key files.

### `pkg/graphengine/`

Contains the graph compiler, executor, and all supporting sub-systems. It is organized into focused sub-packages, each responsible for one stage of the resource graph lifecycle.

**`pkg/graphengine/compiler/`** — RGD Compiler

Translates a `ResourceGraphDefinition` YAML schema into a compiled, executable graph program. Key files:
- `compiler.go` — entry point that orchestrates the full compilation pipeline.
- `typecheck.go` — validates that field references between resources are type-safe.
- `typeinfer.go` — infers data types for dynamic CEL expression fields.
- `validation.go` — checks for structural errors such as missing selectors or invalid patches.
- `program.go` — defines the `Program` struct that represents a compiled graph ready for execution.
- `context.go` — builds the compilation context used across all compiler passes.
- `selector.go` — resolves which resource instances to select when evaluating cross-resource references.

**`pkg/graphengine/executor/`** — Graph Executor

Takes a compiled program and runs it against a live Kubernetes cluster. Key files:
- `simple.go` — the primary executor implementation. Applies resource templates, resolves dynamic references, handles patch reclaiming, and manages ApplySet conflicts.
- `executor.go` — defines the `Executor` interface that `simple.go` implements.

**`pkg/graphengine/runtime/`** — Runtime State

Holds the in-memory execution state of a graph while it is being reconciled. Key files:
- `runtime.go` — manages the overall graph runtime, tracking which nodes are ready, progressing, or failed.
- `node.go` — represents a single resource node inside the runtime graph, including its current status and dependency state.
- `collection.go` — handles runtime state for collection-type nodes (one-to-many resource groups).
- `errors.go` — defines typed runtime error values used across the reconciliation loop.

**`pkg/graphengine/registry/`** — Schema Registry

An in-memory store that maps compiled RGD schemas to their graph programs. Key files:
- `registry.go` — stores and retrieves compiled programs, keyed by RGD name and revision.
- `hash.go` — generates content hashes of RGD schemas to detect changes and invalidate cached programs.

**`pkg/graphengine/rgdadapter/`** — RGD Runtime Adapter

Bridges the Kubernetes controller layer and the graph engine. Key files:
- `runtime.go` — adapts live Kubernetes resource events into the graph runtime state.
- `graph_translate.go` — converts a `ResourceGraphDefinition` object into the internal graph representation used by the compiler.
- `status.go` — builds and patches the status fields of the parent RGD instance based on child resource conditions.

**`pkg/graphengine/watchrouter/`** — Event Watch Router

Routes Kubernetes resource watch events to the correct graph nodes that depend on them. Key files:
- `controller.go` — per-resource watch controller that listens for create/update/delete events.
- `coordinator.go` — coordinates multiple watch controllers, mapping resource types to the graph nodes that reference them.
- `manager.go` — starts and stops watch controllers as RGDs are created or removed.
- `event.go` — defines the typed event struct passed from watchers to the graph runtime.

**`pkg/graphengine/schemawatcher/`** — Dynamic Schema Watcher

Watches for changes to the Kubernetes API server's CRD and resource schema registry. Key files:
- `watcher.go` — monitors OpenAPI schema changes and notifies the compiler when resource types are added, updated, or removed from the cluster.

---

### `pkg/dynamiccontroller/` — Dynamic Controller Manager

Manages the lifecycle of per-RGD Kubernetes controllers that kro spins up at runtime without restarting the main process. Key files:
- `dynamic_controller.go` — the individual controller instance that watches and reconciles instances of a specific custom resource type created by an RGD.
- `coordinator.go` — orchestrates all active dynamic controllers: starts new ones when an RGD is created, stops them when an RGD is deleted, and tracks metrics per controller.

---

### `pkg/cel/` — CEL Expression Evaluator

Evaluates [Common Expression Language (CEL)](https://cel.dev) expressions embedded inside RGD schemas (for example `${schema.spec.name}-service`). Sub-packages:
- `ast/` — parses and inspects CEL expression abstract syntax trees.
- `conversion/` — converts between CEL native types and Kubernetes unstructured JSON values.
- `library/` — registers custom CEL functions available inside kro expressions.
- `sentinels/` — defines sentinel values used to detect and propagate unresolved expression results.
- `unstructured/` — helpers for evaluating CEL expressions against Kubernetes `map[string]interface{}` objects.

Key files in the root of `pkg/cel/`:
- `environment.go` — sets up the CEL evaluation environment with Kubernetes type declarations.
- `types.go` — maps Kubernetes OpenAPI schema types to CEL native types.
- `schemas.go` — resolves OpenAPI schemas for resources referenced inside CEL expressions.
- `expression.go` — parses and caches individual CEL expressions for reuse across reconcile calls.

---

### `pkg/apis/` — Status & Condition Logic

Provides typed Kubernetes status condition management used across all kro resources. Key files:
- `condition.go` — defines the base `Condition` struct with `Type`, `Status`, `Reason`, and `Message` fields.
- `condition_types.go` — defines the condition type constants used by kro (`Ready`, `Progressing`, `Degraded`).
- `condition_set.go` — manages a set of conditions on a resource, handles merging, and exposes helper methods to set or clear specific conditions.

---

### `pkg/applyset/` — Atomic Resource Applicator

Implements the [ApplySet specification](https://kubernetes.io/docs/reference/labels-annotations-taints/#applyset-kubernetes-io-id) for atomically creating, updating, and pruning child Kubernetes resources that belong to a graph instance. Key files:
- `spec.go` — defines the ApplySet spec struct and its label/annotation conventions.



---

## Local Debugging & IDE Setup

You can attach a debugger directly to the kro controller using [Delve](https://github.com/go-delve/delve), which lets you set breakpoints and step through reconciliation logic in real time.

### Debugging with VS Code

Create or update `.vscode/launch.json` at the repository root:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Debug kro Controller",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/controller",
      "args": ["--zap-log-level=debug"],
      "env": {},
      "showLog": true
    }
  ]
}
```

Press **F5** in VS Code to start the controller with the debugger attached.

> [!TIP]
> **Useful breakpoint locations:**
> - `pkg/graphengine/compiler/compiler.go` — step through RGD compilation.
> - `pkg/graphengine/executor/simple.go` — trace live resource reconciliation.
> - `pkg/dynamiccontroller/dynamic_controller.go` — observe controller start/stop events.

### Debugging with GoLand / IntelliJ

1. Open **Run → Edit Configurations...**.
2. Click **+** and choose **Go Build**.
3. Set **Package path** to `github.com/kubernetes-sigs/kro/cmd/controller`.
4. Set **Program arguments** to `--zap-log-level=debug`.
5. Click the **Debug** button (bug icon) to start a live debugging session.

### Verbose Logging Without a Debugger

If you prefer log-based debugging, run the controller with the `debug` log level:

```bash
go run ./cmd/controller --zap-log-level=debug
```

This prints detailed reconciliation events, CEL evaluation steps, and graph execution traces to stdout — useful for quick iteration without attaching a full debugger.