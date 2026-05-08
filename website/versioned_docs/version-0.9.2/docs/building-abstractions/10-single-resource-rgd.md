---
sidebar_position: 10
---

# Single Resource RGD

A single-resource RGD puts a simpler API in front of one underlying resource.
That resource might be a native Kubernetes object like a `Deployment`, or a
provider CRD like an ACK `Bucket` or `DBInstance`. The point is to publish a
better API than the raw resource already provides.

## Why Wrap A Single Resource

Wrapping one resource is a good fit when you want to:

- Hide provider-specific or controller-specific implementation details
- Expose only the small set of inputs users should actually control
- Apply platform defaults for labels, tags, naming, probes, security, or
  networking
- Standardize resources that have unusual or inconsistent status shapes
- Change the underlying implementation later without changing the user-facing
  API
- Define readiness semantics that match the platform contract, not necessarily
  the child resource's native semantics
- Define a clear readiness contract with `readyWhen`
- Project a stable, easy-to-consume status back to users

In other words, the wrapper becomes the public contract. The wrapped resource is
just the implementation behind that contract, and that implementation can
change over time without forcing users to adopt a new API.

## Typical Use Cases

### 1. Publish A Simpler API Over A Native Kubernetes Resource

Sometimes the underlying resource is standard Kubernetes, but still too
low-level or annotation-heavy for your platform contract.

Example: wrap a `Service` of type `LoadBalancer` as `PublicService`:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: publicservice.platform
spec:
  schema:
    apiVersion: v1alpha1
    kind: PublicService
    spec:
      name: string
      app: string
      port: integer | default=8080
      scheme: string | default="internet-facing"
    status:
      published: ${service.status.?loadBalancer.?ingress.size() > 0}
      dnsName: ${service.status.?loadBalancer.?ingress[0].?hostname.orValue("")}

  resources:
    - id: service
      readyWhen:
        - ${service.status.?loadBalancer.?ingress.size() > 0}
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
          annotations:
            service.beta.kubernetes.io/aws-load-balancer-scheme: ${schema.spec.scheme}
            service.beta.kubernetes.io/aws-load-balancer-type: nlb
        spec:
          type: LoadBalancer
          selector:
            app.kubernetes.io/name: ${schema.spec.app}
          ports:
            - port: ${schema.spec.port}
              targetPort: ${schema.spec.port}
```

Here the wrapper owns the cloud-specific annotations, defines `Ready` as "an
external address has been assigned", and projects a simple DNS name into
status.

### 2. Hide A Provider CRD Behind A Consumer-Facing Contract

Third-party resources often expose too many knobs and controller-specific
status. A wrapper lets you expose only the fields your users should care about.

Example: wrap an ACK `DBInstance` as `AppDatabase`:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: appdatabase.platform
spec:
  schema:
    apiVersion: v1alpha1
    kind: AppDatabase
    spec:
      name: string
      storageGB: integer | default=20
      instanceClass: string | default="db.t3.micro"
    status:
      ready: ${database.status.?endpoint.?address.orValue("") != ""}
      endpoint: ${database.status.?endpoint.?address.orValue("")}

  resources:
    - id: database
      readyWhen:
        - ${database.status.?endpoint.?address.orValue("") != ""}
      template:
        apiVersion: rds.services.k8s.aws/v1alpha1
        kind: DBInstance
        metadata:
          name: ${schema.spec.name}
        spec:
          engine: postgres
          dbInstanceIdentifier: ${schema.spec.name}
          allocatedStorage: ${schema.spec.storageGB}
          dbInstanceClass: ${schema.spec.instanceClass}
```

Users interact with `AppDatabase`, not the ACK resource directly. The wrapper
hides provider-specific fields, exposes a stable endpoint in status, and makes
the wrapper instance `Ready` when the database is actually reachable by name.

### 3. Keep The User API Stable While The Implementation Changes

A wrapper also gives you room to change the backing implementation later
without exposing that change to users.

Example: the public `TeamBucket` API can stay the same even if the wrapped
bucket implementation changes:

```kro
# User-facing contract stays stable
schema:
  apiVersion: v1alpha1
  kind: TeamBucket
  spec:
    name: string
  status:
    bucketName: ${bucket.metadata.name}
```

```kro
# Today: TeamBucket wraps an internal MinIO bucket CRD (illustrative)
resources:
  - id: bucket
    template:
      apiVersion: storage.internal.example.com/v1alpha1
      kind: MinIOBucket
      metadata:
        name: ${schema.spec.name}
```

```kro
# Later: same TeamBucket API, backed by ACK S3 Bucket
resources:
  - id: bucket
    template:
      apiVersion: s3.services.k8s.aws/v1alpha1
      kind: Bucket
      metadata:
        name: ${schema.spec.name}
      spec:
        name: ${schema.spec.name}
```

The important part is that users still create `TeamBucket` with the same schema
and consume the same status fields. Only the backing resource behind the
wrapper changes.

### 4. Enforce Naming Conventions From Namespace And Labels

Sometimes the main value of a wrapper is not the spec shape but the naming
policy. A wrapper can derive resource names from the instance namespace, labels,
and a user-supplied base name so teams do not handcraft names differently.

Example: wrap a `ServiceAccount` as `TeamServiceAccount`:

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: teamserviceaccount.platform
spec:
  schema:
    apiVersion: v1alpha1
    kind: TeamServiceAccount
    spec:
      name: string
    status:
      generatedName: ${serviceAccount.metadata.name}

  resources:
    - id: serviceAccount
      template:
        apiVersion: v1
        kind: ServiceAccount
        metadata:
          namespace: ${schema.metadata.namespace}
          name: ${schema.metadata.namespace + "-" + schema.metadata.?labels["team"].orValue("platform") + "-" + schema.spec.name + "-sa"}
```

Here the wrapper enforces a consistent prefix/suffix convention based on the
instance namespace and team label, while still exposing the final generated name
back in status.

### 5. Always Inject Provider Tags With `merge()`

For provider resources, wrappers can also enforce mandatory tags so every
resource carries platform metadata such as tenant, namespace, cost center, or
ownership.

Example: inject platform tags into an ACK `Cluster` while still allowing callers
to pass extra tags:

```kro
schema:
  apiVersion: v1alpha1
  kind: TaggedCluster
  spec:
    name: string
    tags: 'map[string]string | default={}'

resources:
  - id: cluster
    template:
      apiVersion: eks.services.k8s.aws/v1alpha1
      kind: Cluster
      metadata:
        name: ${schema.spec.name}
      spec:
        # other required fields omitted for brevity
        tags: ${merge(schema.spec.tags, {
          "managed-by": "kro",
          "tenant": schema.metadata.?labels["tenant"].orValue("shared"),
          "namespace": schema.metadata.namespace
        })}
```

Putting the platform tags in the second argument to `merge()` makes them win on
key conflicts, so the wrapper always injects the tags you require.

## When Not To Wrap

Wrapping is usually the wrong fit when:

- The underlying resource already matches the platform contract well enough
- Users still need direct access to most of the child resource's fields and
  semantics
- The wrapper is mostly a pass-through that renames fields without removing
  meaningful complexity
- Different teams need materially different lifecycle, security, or policy
  behavior that one shared wrapper would force together
- You are not prepared to own API evolution for the wrapper as the underlying
  resource changes over time

In those cases, letting users work with the native resource directly is often
cleaner than publishing another API surface that adds little real value.

:::warning Limitations and Future Work
kro currently supports [breaking change detection](/docs/concepts/rgd/overview#breaking-changes)
for schema fields (field removal, type changes, new required fields, enum
restrictions, pattern changes) and blocks them by default. However, several
areas are still evolving:

- **Versioning** — multi-version CRD support and migration paths are not yet
  available. See [KREP-009](https://github.com/kubernetes-sigs/kro/pull/935).
- **Resource lifecycle** — fine-grained control over create/update/delete
  behavior per resource is planned. See
  [KREP-014](https://github.com/kubernetes-sigs/kro/pull/1091).
- **Deletion policy** — configurable owner-reference and deletion semantics are
  under proposal. See
  [KREP-004](https://github.com/kubernetes-sigs/kro/pull/763).

Keep these gaps in mind when deciding whether to publish a wrapper — you will
own its API surface and any future migrations.
:::

## Single Resource RGD vs. Grouping

Single-resource abstractions and grouping are related, but they solve different
problems:

- **Single resource** means an RGD provides a better API in front of one
  lower-level resource
- **Grouping** means an RGD manages several tightly-coupled resources as one
  unit

Use a single-resource abstraction when you want to standardize one resource
behind a clean platform API. Use grouping when the value comes from packaging
related resources together.

For more, see [Multi Resource RGD](./20-multi-resource-rgd.md) and
[RGD Chaining](./30-rgd-chaining.md).

## Operational Notes

- If you wrap a third-party CRD, both the CRD and its controller must already be
  installed in the cluster
- In `aggregation` RBAC mode, kro needs permissions for the wrapped resource
  type. See [Access Control](../advanced/01-access-control.md)
- Once users depend on the wrapper, you own its upgrade story. If the wrapped
  resource changes required fields, status shape, or version, evolve the wrapper
  deliberately instead of silently breaking consumers
- Prefer additive schema changes when possible. If you need to make a breaking
  change to the wrapper contract, plan an explicit migration. kro does not
  provide multi-version managed APIs today, so breaking changes usually mean
  introducing a new API surface instead of bumping versions in place
- Treat the wrapper instance as the source of truth; direct edits to the wrapped
  resource are drift
- Surface the outputs users actually need so they do not have to inspect the
  underlying resource during normal workflows
