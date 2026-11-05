---
sidebar_position: 20
---

# Multi Resource RGD

Resource grouping is the pattern of using one RGD to manage several
lower-level resources that belong together as one logical unit. Unlike
[single-resource abstractions](./10-single-resource-rgd.md), which put a better
API in front of one resource, grouping is about packaging a set of related
resources that should always be created, updated, and deleted together.

A good grouping example is a data store plus the access package that always
comes with it. Users do not want to create a table, then hand-write IAM JSON,
then wire a role to that policy. They want one platform object that gives them
the whole package.

## When Grouping Fits Well

Resource grouping is a good fit when:

- The resources are incomplete or unsafe on their own
- One resource's configuration depends on another resource's status or ARN
- You want security and access control to ship with the primary resource
- You want one lifecycle for a whole capability, not separate lifecycles for
  its parts
- You want to hide wiring, ordering, and policy generation from end users

Typical grouped bundles include:

- `Table + IAM Policy + IAM Role`
- `Bucket + bucket policy + access role`
- `Queue + DLQ + redrive policy + consumer policy`
- `Deployment + Service + HPA + PDB + NetworkPolicy`
- `Addon + ServiceAccount + IAM Role`

## Example: AppTable

This example defines an `AppTable` API that always provisions a DynamoDB table
plus the IAM policy and IAM role needed to access it.

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: apptable.platform
spec:
  schema:
    apiVersion: v1alpha1
    kind: AppTable
    spec:
      name: string
      servicePrincipal: string | default="lambda.amazonaws.com"
    status:
      tableName: ${table.spec.tableName}
      tableArn: ${table.status.ackResourceMetadata.arn}
      roleArn: ${role.status.ackResourceMetadata.arn}

  resources:
    - id: table
      template:
        apiVersion: dynamodb.services.k8s.aws/v1alpha1
        kind: Table
        metadata:
          name: ${schema.spec.name}
        spec:
          keySchema:
            - attributeName: id
              keyType: HASH
          attributeDefinitions:
            - attributeName: id
              attributeType: S
          billingMode: PAY_PER_REQUEST
          tableName: ${schema.spec.name}

    - id: accessPolicy
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Policy
        metadata:
          name: ${schema.spec.name}-table-access
        spec:
          name: ${schema.spec.name}-table-access
          policyDocument: |
            {
              "Version": "2012-10-17",
              "Statement": [
                {
                  "Effect": "Allow",
                  "Action": [
                    "dynamodb:GetItem",
                    "dynamodb:PutItem",
                    "dynamodb:UpdateItem",
                    "dynamodb:DeleteItem",
                    "dynamodb:Query",
                    "dynamodb:Scan",
                    "dynamodb:DescribeTable"
                  ],
                  "Resource": [
                    "${table.status.ackResourceMetadata.arn}",
                    "${table.status.ackResourceMetadata.arn}/index/*"
                  ]
                }
              ]
            }

    - id: role
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Role
        metadata:
          name: ${schema.spec.name}-table-role
        spec:
          name: ${schema.spec.name}-table-role
          policies:
            - ${accessPolicy.status.ackResourceMetadata.arn}
          assumeRolePolicyDocument: >
            {
              "Version": "2012-10-17",
              "Statement": [
                {
                  "Effect": "Allow",
                  "Principal": {
                    "Service": "${schema.spec.servicePrincipal}"
                  },
                  "Action": "sts:AssumeRole"
                }
              ]
            }
```

The table is the primary resource, but the group is the real product. The
policy is derived from the actual table ARN, and the role is wired to the
policy automatically. Users consume one `AppTable` object and get a usable
access package with it.

## Why Group Instead Of Exposing Separate Resources

Grouping adds value beyond just putting multiple manifests in one place:

- **Lifecycle coupling**: the table, policy, and role are created and deleted
  together
- **Least-privilege by default**: the policy is generated from the actual table
  ARN instead of being hand-written separately
- **Dependency management**: kro waits for the table ARN before rendering the
  policy, and for the policy ARN before rendering the role
- **Status aggregation**: users can read `tableArn` and `roleArn` from one
  top-level object
- **Safer APIs**: users do not need to understand IAM policy JSON or resource
  ordering

## Useful Grouping Patterns

### Security Bundles

Group a primary resource with the roles, policies, and annotations required to
access it safely.

Examples:

- `Table + IAM Policy + IAM Role`
- `Bucket + IAM Policy + IAM Role`
- `EKS Addon + ServiceAccount + IAM Role`

### Supporting Infrastructure

Group a primary runtime resource with the supporting resources that make it
operational.

Examples:

- `Queue + DLQ + redrive policy`
- `Deployment + Service + HPA + PDB`
- `Cluster + Nodegroup + Addon`

### Conditional Packages

Some groups expand only when a capability is enabled. The group stays one API,
but includes more resources when needed.

Examples:

- `Table + stream consumer role` only when streams are enabled
- `Bucket + replication policy` only when replication is enabled
- `Deployment + Ingress` only when public access is enabled

## When Grouping Does Not Fit

Grouping is usually the wrong fit when:

- The child resources need independent create, update, or delete lifecycles
- Teams need to reuse the parts in different combinations instead of always as
  one package
- The value is really just a simpler API for one primary resource, which is a
  better fit for a [single-resource abstraction](./10-single-resource-rgd.md)
- The bundle mixes unrelated concerns only because the same team happens to own
  them today

If the resources should stay independently consumable, prefer separate APIs or
compose them with chaining instead of forcing them into one permanently coupled
unit.

## Grouping vs Single Resource vs Chaining

- **Single resource** means one RGD puts a better API in front of one
  lower-level resource
- **Grouping** means one RGD manages several tightly-coupled resources as one
  unit
- **Chaining** means one RGD reuses other RGDs as building blocks

Use grouping when the value comes from packaging related resources that should
always travel together. Use a single-resource abstraction when the value comes
from redefining the API for one resource. Use chaining when the building blocks
should be reusable as independent APIs.

For related patterns, see [Single Resource RGD](./10-single-resource-rgd.md)
and [RGD Chaining](./30-rgd-chaining.md).

## Operational Notes

- In `aggregation` RBAC mode, kro needs permissions for every resource type in
  the group, not just the primary resource. See [Access Control](../advanced/01-access-control.md)
- A grouped API is still a public contract. When one child resource changes
  version, schema, or status shape, you own the upgrade and compatibility work
  for the grouped API
- Prefer additive changes to grouped schemas and status. If the grouped
  contract must break, plan an explicit migration. kro does not provide
  multi-version managed APIs today, so breaking changes usually mean
  introducing a new API surface instead of bumping versions in place
- Keep the grouped API focused on one capability; if the bundle starts serving
  unrelated purposes, split it
- Expose the outputs users actually need from the whole group, not just the
  primary resource
- Be deliberate about ownership boundaries: grouped resources should have one
  clear source of truth, the RGD instance
