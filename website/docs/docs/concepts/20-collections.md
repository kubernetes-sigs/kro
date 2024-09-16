---
sidebar_position: 20
---

# 4. Collections

Collections in Symphony provide a powerful way to manage groups of similar
resources within a **ResourceGroup**. They allow for dynamic creation and
management of multiple instances of a resource type based on user input.

## What are Collections?

A collection is a special field in a **ResourceGroup** that defines a template for
creating multiple similar resources. Key features of collections include:

- Dynamic creation of resources based on user input
- Consistent structure across multiple resource instances
- Simplified management of groups of related resources

## Defining a Collection

Here's an example of how to define a collection in a ResourceGroup:

```yaml
apiVersion: symphony.k8s.aws/v1alpha1
kind: ResourceGroup
metadata:
  name: ReplicaSet.x.symphony.k8s.aws
spec:
  kind: ReplicaSet
  apiVersion: v1alpha1
  parameters:
    spec:
      clusterName: string
      podCount: integer | minimum=1 maximum=5
  resources:
    - name: nodesCollection
      collection:
        index: ${range(0, spec.podCount)}
        definition:
          apiVersion: v1
          kind: Pod
          metadata:
            name: ${clusterName}-node-${index}
          spec:
            containers:
              - name: db
                image: nginx:latest
```

In this example, `nodes` is a collection that will create multiple Pod
resources based on the `podCount` parameter.

## Key Concepts

1. **index**: Specifies the range of values for the collection, allowing for
   dynamic creation of multiple resources.

2. **definition**: Defines the structure of each resource instance in the
   collection. The `${index}` variable ensures uniqueness of each resource.

## Using Collections in Claims

When creating a claim, users can specify the count for the collection:

```yaml
apiVersion: symphony.k8s.aws/v1alpha1
kind: ReplicaSet
metadata:
  name: my-db-cluster
spec:
  clusterName: production-db
  podCount: 3
```

This claim will result in the creation of three Postgres Pods named
`production-db-node-0`, `production-db-node-1`, and `production-db-node-2`.

## Deployment Strategy

While defining collections is straightforward, it's essential to consider the
deployment strategy for managing multiple resources. Symphony provides
flexibility in managing collections, allowing users to define how resources are
created, updated, and deleted based on the desired state.

Symphony provide two strategies for managing collections:
- **RollingUpdate**: Creates, updates and deletes resources in an incremental manner,
  ensuring that only one resource is updated at a time.
- **ParallelUpdate**: Creates, updates and deletes resources in parallel, allowing
  for faster deployment of multiple resources.

For examples you can add the following to the `spec` section of the `ResourceGroup`:

```yaml
spec:
  kind: ReplicaSet
  apiVersion: v1alpha1
  parameters:
    spec:
      clusterName: string
      podCount: integer | minimum=1 maximum=5
  resources:
    - name: nodes
      strategy: RollingUpdate
      collection:
        index: ${range(0, spec.podCount)}
        definition:
          apiVersion: v1
          kind: Pod
          metadata:
            name: ${clusterName}-node-${index}
          spec:
            containers:
              - name: db
                image: nginx:latest
```