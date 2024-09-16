---
sidebar_position: 10
---

# EKSCluster

```yaml title="ekscluster-rg.yaml"
apiVersion: x.symphony.k8s.aws/v1alpha1
kind: ResourceGroup
metadata:
  name: ekscluster.x.symphony.k8s.aws
spec:
  # CRD Definition
  apiVersion: v1alpha1
  kind: EKSCluster

  definition:
    spec:
      name: string
      version: string
      numNodes: integer
      
  # resources
  resources:
  - name: clusterVPC
    definition:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: VPC
      metadata:
        name: cluster-vpc-${spec.name}
      spec:
        cidrBlocks:
        - 192.168.0.0/16
        enableDNSHostnames: false
        enableDNSSupport: true

  - name: subnetAZA
    definition:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: Subnet
      metadata:
        name: cluster-subnet-a-${spec.name}
      spec:
        availabilityZone: us-west-2a
        cidrBlock: 192.168.0.0/18
        vpcID: ${clusterVPC.status.vpcID}

  - name: securityGroup
    definition:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: SecurityGroup
      metadata:
        name: cluster-security-group-${spec.name}
      spec:
        vpcID: ${clusterVPC.status.vpcID}
        name: my-eks-cluster-sg-${spec.name}
        description: something something

  - name: subnetAZB
    definition:
      apiVersion: ec2.services.k8s.aws/v1alpha1
      kind: Subnet
      metadata:
        name: cluster-subnet-b-${spec.name}
      spec:
        availabilityZone: us-west-2b
        cidrBlock: 192.168.64.0/18
        vpcID: ${clusterVPC.status.vpcID}

  - name: clusterRole
    definition:
      apiVersion: iam.services.k8s.aws/v1alpha1
      kind: Role
      metadata:
        name: cluster-role-${spec.name}
      spec:
        name: cluster-role-${spec.name}
        policies:
        - arn:aws:iam::aws:policy/AmazonEKSClusterPolicy
        assumeRolePolicyDocument: |
          {
            "Version": "2012-10-17",
            "Statement": [
              {
                "Effect": "Allow",
                "Principal": {
                  "Service": "eks.amazonaws.com"
                },
                "Action": "sts:AssumeRole"
              }
            ]
          }

  - name: nodeRole
    definition:
      apiVersion: iam.services.k8s.aws/v1alpha1
      kind: Role
      metadata:
        name: cluster-node-role-${spec.name}
      spec:
        name: cluster-node-role-${spec.name}
        policies:
        - arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy
        - arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly
        - arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy
        assumeRolePolicyDocument: |
          {
            "Version": "2012-10-17",
            "Statement": [
              {
                "Effect": "Allow",
                "Principal": {
                  "Service": "ec2.amazonaws.com"
                },
                "Action": "sts:AssumeRole"
              }
            ]
          }

  - name: cluster
    definition:
      apiVersion: eks.services.k8s.aws/v1alpha1
      kind: Cluster
      metadata:
        name: cluster-${spec.name}
      spec:
        name: cluster-${spec.name}
        roleARN: ${clusterRole.status.ackResourceMetadata.arn}
        version: ${spec.version}
        resourcesVPCConfig:
          subnetIDs:
            - ${subnetAZA.status.subnetID}
            - ${subnetAZB.status.subnetID}
            
  - name: nodegroup
    definition:
      apiVersion: eks.services.k8s.aws/v1alpha1
      kind: Nodegroup
      metadata:
        name: nodegroup-${spec.name}
      spec:
        name: nodegroup-${spec.name}
        clusterName: cluster-${spec.name}
        subnets:
          - ${subnetAZA.status.subnetID}
          - ${subnetAZB.status.subnetID}
        nodeRole: ${nodeRole.status.ackResourceMetadata.arn}
        updateConfig:
          maxUnavailable: 1
        scalingConfig:
          minSize: ${spec.numNodes}
          maxSize: ${spec.numNodes}
          desiredSize: ${spec.numNodes}
```