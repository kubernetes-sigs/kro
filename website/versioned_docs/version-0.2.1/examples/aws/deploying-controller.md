---
sidebar_position: 305
---

# Controller Deployment

```yaml title="controller-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: ekscontrollers.kro.run
spec:
  schema:
    apiVersion: v1alpha1
    kind: EKSController
    spec:
      name: string | default=eks-controller
      namespace: string | default=default
      values:
        aws:
          accountID: string | required=true
          region: string | default=us-west-2
        deployment:
          containerPort: integer | default=8080
          replicas: integer | default=1
        iamRole:
          maxSessionDuration: integer | default=3600
          oidcProvider: string | required=true
          roleDescription: string | default=IRSA role for ACK EKS controller deployement on EKS cluster using kro Resource group
        iamPolicy:
          # would prefer to add a policyDocument here, need to support multiline string here
          description: string | default="policy for eks controller"
        image:
          deletePolicy: string | default=delete
          repository: string | default=public.ecr.aws/aws-controllers-k8s/eks-controller
          tag: string | default=1.4.7
          resources:
            requests:
              memory: string | default=64Mi
              cpu: string | default=50m
            limits:
              memory: string | default=128Mi
              cpu: string | default=100m
        log:
          enabled: boolean | default=false
          level: string | default=info
        serviceAccount:
          name: string | default=eks-controller-sa
  resources:
  - id: eksCRDGroup
    template:
      apiVersion: kro.run/v1alpha1
      kind: EKSCRDGroup
      metadata:
        name: ${schema.spec.name}-crd-group
      spec:
        name: ${schema.spec.name}-crd-group
  - id: eksControllerIamPolicy
    template:
      apiVersion: iam.services.k8s.aws/v1alpha1
      kind: Policy
      metadata:
        name: ${schema.spec.name}-iam-policy
      spec:
        name: ${schema.spec.name}-iam-policy
        description: ${schema.spec.values.iamPolicy.description}
        policyDocument: >
          {
            "Version": "2012-10-17",
            "Statement": [
              {
                "Sid": "VisualEditor0",
                "Effect": "Allow",
                "Action": [
                  "eks:*",
                  "iam:GetRole",
                  "iam:PassRole",
                  "iam:ListAttachedRolePolicies",
                  "ec2:DescribeSubnets"
                ],
                "Resource": "*"
              }
            ]
          }
  - id: eksControllerIamRole
    template:
      apiVersion: iam.services.k8s.aws/v1alpha1
      kind: Role
      metadata:
        name: ${schema.spec.name}-iam-role
        namespace: ${schema.spec.namespace}
      spec:
        name: ${schema.spec.name}-iam-role
        description: ${schema.spec.values.iamRole.roleDescription}
        maxSessionDuration: ${schema.spec.values.iamRole.maxSessionDuration}
        policies:
        - ${eksControllerIamPolicy.status.ackResourceMetadata.arn}
        assumeRolePolicyDocument: >
          {
            "Version":"2012-10-17",
            "Statement": [{
              "Effect":"Allow",
              "Principal": {"Federated": "arn:aws:iam::${schema.spec.values.aws.accountID}:oidc-provider/${schema.spec.values.iamRole.oidcProvider}"},
              "Action": ["sts:AssumeRoleWithWebIdentity"],
              "Condition": {
                "StringEquals": {"${schema.spec.values.iamRole.oidcProvider}:sub": "system:serviceaccount:${schema.spec.namespace}:${schema.spec.values.serviceAccount.name}"}
              }
            }]
          }
  - id: serviceAccount
    template:
      apiVersion: v1
      kind: ServiceAccount
      metadata:
        name: ${schema.spec.values.serviceAccount.name}
        namespace: ${schema.spec.namespace}
        annotations:
          eks.amazonaws.com/role-arn : ${eksControllerIamRole.status.ackResourceMetadata.arn}
  - id: deployment
    template:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${schema.spec.name}-deployment
        namespace: ${schema.spec.namespace}
        labels:
          app.kubernetes.io.name: ${schema.spec.name}-deployment
          app.kubernetes.io.instance: ${schema.spec.name}
      spec:
        replicas: ${schema.spec.values.deployment.replicas}
        selector:
          matchLabels:
            app.kubernetes.io.name: ${schema.spec.name}-deployment
            app.kubernetes.io.instance: ${schema.spec.name}
        template:
          metadata:
            labels:
              app.kubernetes.io.name: ${schema.spec.name}-deployment
              app.kubernetes.io.instance: ${schema.spec.name}
          spec:
            serviceAccountName: ${serviceAccount.metadata.name}
            containers:
            - command:
              - ./bin/controller
              args:
              - --aws-region
              - ${schema.spec.values.aws.region}
              - --enable-development-logging=${schema.spec.values.log.enabled}
              - --log-level
              - ${schema.spec.values.log.level}
              - --deletion-policy
              - ${schema.spec.values.image.deletePolicy}
              - --watch-namespace
              - ${schema.spec.namespace}
              image: ${schema.spec.values.image.repository}:${schema.spec.values.image.tag}
              name: controller
              ports:
                - name: http
                  containerPort: ${schema.spec.values.deployment.containerPort}
              resources:
                requests:
                  memory: ${schema.spec.values.image.resources.requests.memory}
                  cpu: ${schema.spec.values.image.resources.requests.cpu}
                limits:
                    memory: ${schema.spec.values.image.resources.limits.memory}
                    cpu: ${schema.spec.values.image.resources.limits.cpu}
              env:
              - name: ACK_SYSTEM_NAMESPACE
                value: ${schema.spec.namespace}
              - name: AWS_REGION
                value: ${schema.spec.values.aws.region}
              - name: DELETE_POLICY
                value: ${schema.spec.values.image.deletePolicy}
              - name: ACK_LOG_LEVEL
                value: ${schema.spec.values.log.level}
              ports:
              - containerPort: 80
  - id: clusterRoleBinding
    template:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRoleBinding
      metadata:
        name: ${schema.spec.name}-clusterrolebinding
      roleRef:
        kind: ClusterRole
        apiGroup: rbac.authorization.k8s.io
        name: ${clusterRole.metadata.name}
      subjects:
      - kind: ServiceAccount
        name: ${serviceAccount.metadata.name}
        namespace: ${serviceAccount.metadata.namespace}
  - id: clusterRole
    template:
      apiVersion: rbac.authorization.k8s.io/v1
      kind: ClusterRole
      metadata:
        name: ${schema.spec.name}-clusterrole
      rules:
      - apiGroups:
        - ""
        resources:
        - configmaps
        - secrets
        verbs:
        - get
        - list
        - patch
        - watch
      - apiGroups:
        - ""
        resources:
        - namespaces
        verbs:
        - get
        - list
        - watch
      - apiGroups:
        - ec2.services.k8s.aws
        resources:
        - securitygroups
        - securitygroups/status
        - subnets
        - subnets/status
        verbs:
        - get
        - list
      - apiGroups:
        - eks.services.k8s.aws
        resources:
        - accessentries
        - addons
        - clusters
        - fargateprofiles
        - identityproviderconfigs
        - nodegroups
        - podidentityassociations
        verbs:
        - create
        - delete
        - get
        - list
        - patch
        - update
        - watch
      - apiGroups:
        - eks.services.k8s.aws
        resources:
        - accessentries/status
        - addons/status
        - clusters/status
        - fargateprofiles/status
        - identityproviderconfigs/status
        - nodegroups/status
        - podidentityassociations/status
        verbs:
        - get
        - patch
        - update
      - apiGroups:
        - iam.services.k8s.aws
        resources:
        - roles
        - roles/status
        verbs:
        - get
        - list
      - apiGroups:
        - kms.services.k8s.aws
        resources:
        - keys
        - keys/status
        verbs:
        - get
        - list
      - apiGroups:
        - services.k8s.aws
        resources:
        - adoptedresources
        - fieldexports
        verbs:
        - create
        - delete
        - get
        - list
        - patch
        - update
        - watch
      - apiGroups:
        - services.k8s.aws
        resources:
        - adoptedresources/status
        - fieldexports/status
        verbs:
        - get
        - patch
        - update
```
