# KRO CLI

## Existing CLI

A `kro` CLI currently exists in the codebase at `cmd/kro/` with the following commands:

**Generate commands:**
- `kro generate crd -f rgd.yaml` - Generates the CRD manifest from a ResourceGraphDefinition
- `kro generate instance -f rgd.yaml` - Generates a skeleton instance manifest with default values
- `kro generate diagram -f rgd.yaml` - Generates an interactive HTML diagram of the resource dependency graph

**Validate commands:**
- `kro validate rgd -f rgd.yaml` - Validates a ResourceGraphDefinition file (requires API server)

## Problem statement

The current `kro` CLI is limited and does not solve some user problems. The `kro` CLI is also not very accessible, requiring customers to build it from source. 

One problem is packaging and sharing RGDs. One powerful aspect of the Kubernetes community is you can take advantage of prior work. You don't need to write your own Grafana Helm chart, you can pull one from a registry. We should enable users to do the same with packaging and publishing RGD definitions. We should also let users package and share RGDs for internal use too for the enterprise use case.

Another general problem is validation before actions are taken. Users currently need to perform actions to know if they are successful. In order to know if an RGD is valid you need to actually create that RGD. It would be helpful to be able to validate an RGD or an update to RGD would complete and run as expected. Visualizing the changes that would apply to a cluster corresponding to a given update would allow users to make changes with more confidence that they are doing the right thing.   

Another problem is the learning curve for new users working with Kro. Some actions like listing all instances of an RGD may require you to identify the corresponding CR and list that.

A final problem is that Kro maintainers want to try out experimental features and get feedback on them. An example of something that could fit into this category would be workflows to convert RGDs to and from Helm charts. 

## Proposal

We will improve existing commands and add new commands to the current CLI. We will also make it more accessible for customers by adding it to the release.

#### Overview

All updates will be to the existing `kro` CLI tool and codebase.

#### Command Summary

The following table summarizes all commands that the `kro` CLI will support:

| Command | Description | Status |
|---------|-------------|--------|
| `kro validate rgd` | Validate a ResourceGraphDefinition file | Existing but needs update |
| `kro validate instance` | Validate both an RGD and instance are valid | New |
| `kro registry login` | Authenticate to OCI registries | New |
| `kro push` | Package and push an RGD to an OCI registry | New |
| `kro pull` | Pull an RGD from an OCI registry | New |
| `kro install` | Pull an RGD from registry and apply it to cluster | New |
| `kro preview` | Show diff of resources/RGD changes (works for both instances and RGDs) | New |
| `kro generate crd` | Generate CRD manifest from a ResourceGraphDefinition | Existing |
| `kro generate instance` | Generate skeleton instance manifest with default values | Existing |
| `kro generate diagram` | Generate interactive HTML diagram of resource dependency graph | Existing |

#### Design details

The following sections are roughly in order of priority and are planned to be delivered incrementally.

**Note on existing commands:** The existing `generate` commands (crd, instance, diagram) will remain unchanged. This proposal only modifies the `validate` command and adds new commands.

##### Validate

The intent of validation commands is to allow users to catch errors from RGDs earlier. These might be good to run as part of a build check in a PR in a GitOps setup. All validation commands will not require access to the internet or Kubernetes API server.

**Note:** This changes the existing `validate rgd` behavior from requiring API server access to working offline by default. A `--discoverFromAPIServer` flag will be added for users who need the old behavior.

Update existing `validate` command to work without Kubernetes API server access.
```bash
kro validate rgd -f rgd-file.yaml \
   --crds ./directoryToCRDS \ # Optional offline CRD directory
   --kubernetesVersion 1.30   # Optional flag to tell what Schemas to use for built in kinds
   --discoverFromAPIServer    # Optional flag to discover CRDs and Kubernetes version from API server
```

Add a new command to validate that both an RGD and instance are valid.
```bash
kro validate instance --rgd-file rgd-file.yaml --instance-file instance-file.yaml \
   --crds ./directoryToCRDS \ # Optional offline CRD directory
   --kubernetesVersion 1.30 \ # Optional flag to tell what Schemas to use for built in kinds
   --discoverFromAPIServer    # Optional flag to discover CRDs and Kubernetes version from API server
```

##### Packaging

The goal of packaging is to allow users to pull and publish RGDs to OCI registries. These will all be new commands.

###### Packaging auth

We will need to authenticate to OCI registries. The `kro` CLI will look in two places for credentials in this order:

1. creds set by `kro registry login` at `~/.kro/config.json`
2. creds set by `docker login`

This is very similar to Helm's handling of auth with `~/.config/helm/registry/config.json`. 

The registry login command is defined here:
```bash
kro registry login registry.io \
   -u username \                  # Registry username
   -p password \                  # Registry password or identity token
   --password-stdin \             # Read password from stdin
   --cert-file ./client.crt \     # SSL certificate file
   --key-file ./client.key \      # SSL key file
   --ca-file ./ca.crt \           # CA bundle
   --insecure \                   # Allow connections without certs
   --ecr                          # Use AWS ECR authentication with AWS cred chain
```

We could rely just on `docker login` creds but providing an explicit login avoids users needing to have docker installed while maintaining the flexibility of supporting all providers docker supports.

Initially we will provide an ```ecr``` flag to make integration with ```ACK``` and ```AWS``` smoother. We can add other cloud provider flags later as requested from users.

###### Packaging commands

All artifacts will be stored in container registries. Since RGDs are just a single file, we will not have the option to create immediate `.tar` files like `helm package` does.

Packages and pushes an RGD to an OCI registry.
```bash
kro push -f rgd.yaml registry.io/org/my-rgd:v1.0.0
```

Reads an RGD file from a registry.
```bash
kro pull registry.io/org/my-rgd:v1.0.0 -o rgd.yaml
```

Helper to pull the RGD then kubectl apply it.
```bash
kro install registry.io/org/my-rgd:v1.0.0
```

#### Dry run for changes

`kro preview` will be the mechanism for dry running changes. Preview will require access to a Kubernetes API server and will perform client-side diffs against the live cluster state.

##### preview instance

This can be run on an instance. It will show the diff YAML of the resources that will be created/updated.
```bash
kro preview -f instance.yaml

# Example output:
CREATE: Deployment my-app-deployment (apps/v1)
---
+ apiVersion: apps/v1
+ kind: Deployment
+ metadata:
+   name: my-app-deployment
+ spec:
+   replicas: 3
+   selector:
+     matchLabels:
+       app: my-app

UPDATE: Service my-app-service (v1)
---
  apiVersion: v1
  kind: Service
  metadata:
    name: my-app-service
  spec:
-   port: 8080
+   port: 9090

DELETE: ConfigMap old-config (v1)
---
- apiVersion: v1
- kind: ConfigMap
- metadata:
-   name: old-config
- data:
-   key: value
```

##### preview rgd

This can be run on an RGD. It will show:
- The diff of the RGD YAML that will be created/updated
- The diff of any instances that will be updated
```bash
kro preview -f rgd.yaml

# Example output:
UPDATE: ResourceGroup my-app (kro.run/v1alpha1)
---
  apiVersion: kro.run/v1alpha1
  kind: ResourceGroup
  metadata:
    name: my-app
  spec:
    schema:
      apiVersion: v1alpha1
      kind: MyApp
      spec:
-       replicas: int
+       replicas: int | default=3

Affected instances (2):
- my-app-prod
- my-app-dev

UPDATE: Deployment my-app-prod-deployment (apps/v1) [from instance: my-app-prod]
---
  spec:
-   replicas: 1
+   replicas: 3

UPDATE: Deployment my-app-dev-deployment (apps/v1) [from instance: my-app-dev]
---
  spec:
-   replicas: 1
+   replicas: 3
```

#### CLI Binary Distribution

We will use GoReleaser to build and publish CLI binaries (linux, darwin, windows for amd64/arm64) as part of GitHub releases. We're choosing GoReleaser over simple `go build` commands because it will make it easier to add distribution targets later (Homebrew, package managers, etc.).

The `kro` CLI will be "experimental" for at least 3 Kro releases and will not offer any guarantee of avoiding breakages. We will version the `kro` CLI to follow the same as the `kro` project initially. In the future it will likely make sense to decouple these.

Since Kro is a fast evolving project we will warn users if their `kro` CLI version does not match their Kro version for applicable commands. Initially we will not support any skew between the Kro version and CLI version.

Users will download binaries directly from GitHub releases:
```bash
# Example for Linux AMD64
curl -LO https://github.com/kubernetes-sigs/kro/releases/download/v0.8.0/kro_0.8.0_linux_amd64.tar.gz
tar -xzf kro_0.8.0_linux_amd64.tar.gz
sudo mv kro /usr/local/bin/
```

#### Docs

We will add a new section to the kro docs with CLI documentation. These docs will be written manually and not be generated off the CLI implementation initially. 

## Other solutions considered

N/A 

A CLI is a pretty common feature for a project like this.

## Scoping

#### What is in scope for this proposal?

The scope for this proposal includes:
- Publishing the `kro` CLI as part of releases
- Updating the existing `validate rgd` command
- Adding a `validate instance` command
- Adding packaging commands
- Adding preview commands

#### What is not in scope?

##### All CLI commands

The list suggested is not exhaustive by any means. We should continue adding to the CLI as new requirements and use cases come up.

##### Adding the `kro` CLI to package managers

Adding the `kro` CLI to various package managers will make it easier for users to install the CLI. For now, we will narrow the scope of how we distribute the CLI to allow faster iteration on it. 

##### Public Kro repos

It's possible to increase adoption of the project we would want to maintain high quality useful applications. 

Many more users install Helm charts than write them. Kro will likely be the same once a robust ecosystem of software exists.

We will not get into maintaining these repos yet.

##### Experimental features

Experimental features like generating RGDs from Helm charts or other conversion workflows are not in scope for this proposal. These may be added later as the CLI matures and we gather user feedback on core functionality.

##### Telemetry

We are not going to initially add telemetry for concerns of user privacy.

We will instead rely on community feedback through GitHub issues and community meetings. We will also be able to look at download counts on the GitHub release page for the CLI to track how many users are trying it out.

## Testing strategy

#### Requirements

We should not need any additional infrastructure to test the CLI in integration tests and unit tests.

#### Test plan

We will write both unit and integration tests for the `kro` CLI to help validate that changes are working as expected.