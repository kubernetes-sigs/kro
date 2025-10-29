---
sidebar_position: 10
---

# EKS Cluster

```yaml title="eks.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: ekscluster.kro.run
spec:
  # CRD Schema
  schema:
    apiVersion: v1alpha1
    kind: EKSCluster
    spec:
      name: string
      version: string
    status:
      networkingInfo:
        vpcID: ${clusterVPC.status.vpcID}
        subnetAZA: ${clusterSubnetA.status.subnetID}
        subnetAZB: ${clusterSubnetB.status.subnetID}
      clusterARN: ${cluster.status.ackResourceMetadata.arn}
  # resources
  resources:
    - id: clusterVPC
      readyWhen:
        - ${clusterVPC.status.state == "available"}
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: VPC
        metadata:
          name: kro-cluster-vpc
        spec:
          cidrBlocks:
            - 192.168.0.0/16
          enableDNSSupport: true
          enableDNSHostnames: true
    - id: clusterElasticIPAddress
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: ElasticIPAddress
        metadata:
          name: kro-cluster-eip
        spec: {}
    - id: clusterInternetGateway
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: InternetGateway
        metadata:
          name: kro-cluster-igw
        spec:
          vpc: ${clusterVPC.status.vpcID}
    - id: clusterRouteTable
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: RouteTable
        metadata:
          name: kro-cluster-public-route-table
        spec:
          vpcID: ${clusterVPC.status.vpcID}
          routes:
            - destinationCIDRBlock: 0.0.0.0/0
              gatewayID: ${clusterInternetGateway.status.internetGatewayID}
    - id: clusterSubnetA
      readyWhen:
        - ${clusterSubnetA.status.state == "available"}
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: Subnet
        metadata:
          name: kro-cluster-public-subnet1
        spec:
          availabilityZone: us-west-2a
          cidrBlock: 192.168.0.0/18
          vpcID: ${clusterVPC.status.vpcID}
          routeTables:
            - ${clusterRouteTable.status.routeTableID}
          mapPublicIPOnLaunch: true
    - id: clusterSubnetB
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: Subnet
        metadata:
          name: kro-cluster-public-subnet2
        spec:
          availabilityZone: us-west-2b
          cidrBlock: 192.168.64.0/18
          vpcID: ${clusterVPC.status.vpcID}
          routeTables:
            - ${clusterRouteTable.status.routeTableID}
          mapPublicIPOnLaunch: true
    - id: clusterNATGateway
      template:
        apiVersion: ec2.services.k8s.aws/v1alpha1
        kind: NATGateway
        metadata:
          name: kro-cluster-natgateway1
        spec:
          subnetID: ${clusterSubnetB.status.subnetID}
          allocationID: ${clusterElasticIPAddress.status.allocationID}
    - id: clusterRole
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Role
        metadata:
          name: kro-cluster-role
        spec:
          name: kro-cluster-role
          description: "kro created cluster cluster role"
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
    - id: clusterNodeRole
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Role
        metadata:
          name: kro-cluster-node-role
        spec:
          name: kro-cluster-node-role
          description: "kro created cluster node role"
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
    - id: clusterAdminRole
      template:
        apiVersion: iam.services.k8s.aws/v1alpha1
        kind: Role
        metadata:
          name: kro-cluster-pia-role
        spec:
          name: kro-cluster-pia-role
          description: "kro created cluster admin pia role"
          policies:
            - arn:aws:iam::aws:policy/AdministratorAccess
          assumeRolePolicyDocument: |
            {
                "Version": "2012-10-17",
                "Statement": [
                    {
                        "Sid": "AllowEksAuthToAssumeRoleForPodIdentity",
                        "Effect": "Allow",
                        "Principal": {
                            "Service": "pods.eks.amazonaws.com"
                        },
                        "Action": [
                            "sts:AssumeRole",
                            "sts:TagSession"
                        ]
                    }
                ]
            }
    - id: cluster
      readyWhen:
        - ${cluster.status.status == "ACTIVE"}
      template:
        apiVersion: eks.services.k8s.aws/v1alpha1
        kind: Cluster
        metadata:
          name: ${schema.spec.name}
        spec:
          name: ${schema.spec.name}
          accessConfig:
            authenticationMode: API_AND_CONFIG_MAP
          roleARN: ${clusterRole.status.ackResourceMetadata.arn}
          version: ${schema.spec.version}
          resourcesVPCConfig:
            endpointPrivateAccess: false
            endpointPublicAccess: true
            subnetIDs:
              - ${clusterSubnetA.status.subnetID}
              - ${clusterSubnetB.status.subnetID}
    - id: clusterNodeGroup
      template:
        apiVersion: eks.services.k8s.aws/v1alpha1
        kind: Nodegroup
        metadata:
          name: kro-cluster-nodegroup
        spec:
          name: kro-cluster-ng
          diskSize: 100
          clusterName: ${cluster.spec.name}
          subnets:
            - ${clusterSubnetA.status.subnetID}
            - ${clusterSubnetB.status.subnetID}
          nodeRole: ${clusterNodeRole.status.ackResourceMetadata.arn}
          updateConfig:
            maxUnavailable: 1
          scalingConfig:
            minSize: 1
            maxSize: 1
            desiredSize: 1
```
