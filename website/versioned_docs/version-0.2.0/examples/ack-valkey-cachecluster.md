---
sidebar_position: 10
---

# Valkey cluster

```yaml title="valkey-cachecluster.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: valkey.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: Valkey
    spec:
      name: string
    status:
      csgARN: ${cacheSubnetGroup.status.ackResourceMetadata.arn}
      subnets: ${cacheSubnetGroup.status.subnets}
      clusterARN: ${valkey.status.ackResourceMetadata.arn}
  resources:
    - id: networkingStack
      template:
        apiVersion: kro.run/v1alpha1
        kind: NetworkingStack
        metadata:
          name: ${schema.spec.name}-networking-stack
        spec:
          name: ${schema.spec.name}-networking-stack
    - id: cacheSubnetGroup
      template:
        apiVersion: elasticache.services.k8s.aws/v1alpha1
        kind: CacheSubnetGroup
        metadata:
          name: ${schema.spec.name}-valkey-subnet-group
        spec:
          cacheSubnetGroupDescription: "Valkey ElastiCache subnet group"
          cacheSubnetGroupName: ${schema.spec.name}-valkey-subnet-group
          subnetIDs:
            - ${networkingStack.status.networkingInfo.subnetAZA}
            - ${networkingStack.status.networkingInfo.subnetAZB}
            - ${networkingStack.status.networkingInfo.subnetAZC}
    - id: sg
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: SecurityGroup
        metadata:
          name: ${schema.spec.name}-valkey-sg
        spec:
          name: ${schema.spec.name}-valkey-sg
          description: "Valkey ElastiCache security group"
          vpcID: ${networkingStack.status.networkingInfo.vpcID}
          ingressRules:
            - fromPort: 6379
              toPort: 6379
              ipProtocol: tcp
              ipRanges:
                - cidrIP: 0.0.0.0/0
    - id: valkey
      template:
        apiVersion: elasticache.services.k8s.aws/v1alpha1
        kind: CacheCluster
        metadata:
          name: ${schema.spec.name}-valkey
        spec:
          cacheClusterID: vote-valkey-cluster
          cacheNodeType: cache.t3.micro
          cacheSubnetGroupName: ${schema.spec.name}-valkey-subnet-group
          engine: valkey
          engineVersion: "8.x"
          numCacheNodes: 1
          port: 6379
          securityGroupIDs:
            - ${sg.status.id}
```
