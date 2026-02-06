---
sidebar_position: 101
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

# S3 Buckets

This example demonstrates how to create and manage AWS S3 buckets using kro's ResourceGraphDefinition.

## Overview

The S3 bucket example shows how to define an S3 bucket resource with common configurations such as versioning, encryption, and access policies. This is useful for applications that need object storage with consistent configuration across environments.
## Manifest files



<Tabs>
  <TabItem value="instance" label="instance.yaml">

```kro
apiVersion: kro.run/v1alpha1
kind: S3Bucket
metadata:
  name: s3demo
  namespace: default
spec:
  name: s3demo-11223344
```

  </TabItem>
  <TabItem value="rg" label="rg.yaml">

```kro
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: s3bucket.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: S3Bucket
    spec:
      name: string
      access: string | default="write"
    status:
      s3ARN: ${s3bucket.status.ackResourceMetadata.arn}
      s3PolicyARN: ${s3PolicyWrite.status.ackResourceMetadata.arn}

  resources:
  - id: s3bucket
    template:
      apiVersion: s3.services.k8s.aws/v1alpha1
      kind: Bucket
      metadata:
        name: ${schema.spec.name}
      spec:
        name: ${schema.spec.name}
  - id: s3PolicyWrite
    includeWhen:
    - ${schema.spec.access == "write"}
    template:
      apiVersion: iam.services.k8s.aws/v1alpha1
      kind: Policy
      metadata:
        name: ${schema.spec.name}-s3-write-policy
      spec:
        name: ${schema.spec.name}-s3-write-policy
        policyDocument: |
          {
            "Version": "2012-10-17",
            "Statement": [
              {
                "Effect": "Allow",
                "Action": [
                  "s3:GetObject",
                  "s3:PutObject",
                  "s3:PutObjectAcl",
                  "s3:DeleteObject"
                ],
                "Resource": [
                  "${s3bucket.status.ackResourceMetadata.arn}/*"
                ]
              },
              {
                "Effect": "Allow",
                "Action": [
                  "s3:ListBucket",
                  "s3:GetBucketLocation"
                ],
                "Resource": [
                  "${s3bucket.status.ackResourceMetadata.arn}"
                ]
              }
            ]
          }
```

  </TabItem>
</Tabs>
