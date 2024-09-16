---
sidebar_position: 2
---

# Deploy a Resource Group

ResourceGroups are the core building blocks of Symphony. They define the
structure of your application and the resources it requires. In this guide,
you'll learn how to deploy a ResourceGroup using Symphony.

## Prerequisites

Before you begin, ensure you have the following:
- Installed Symphony on your Kubernetes cluster
- A ResourceGroup manifest file

For this examole, we'll use a simple ResourceGroup that defines a Deployment
and a Service. Here's an example of a `ResourceGroup` manifest file:

```yaml title="deploymentservice-rg.yaml"
apiVersion: x.symphony.k8s.aws/v1alpha1
kind: ResourceGroup
metadata:
  name: deploymentservice.x.symphony.k8s.aws
spec:
  apiVersion: v1alpha1
  kind: DeploymentService
  definition:
    spec:
      name: string
  resources:
  - name: deployment
    definition:
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: ${spec.name}
      spec:
        replicas: 1
        selector:
          matchLabels:
            app: deployment
        template:
          metadata:
            labels:
              app: deployment
          spec:
            containers:
            - name: ${spec.name}-deployment
              image: nginx
              ports:
              - containerPort: 80
  - name: service
    definition:
      apiVersion: v1
      kind: Service
      metadata:
        name: ${spec.name}
      spec:
        selector:
          app: deployment
        ports:
        - protocol: TCP
          port: 80
          targetPort: 80
```

## Steps

1. **Create a ResourceGroup manifest file**: Create a new file with the
   ResourceGroup definition. You can use the example above as a template.

2. **Deploy the ResourceGroup**: Use the `kubectl` command to deploy the
    ResourceGroup to your Kubernetes cluster:
    
    ```bash
    kubectl apply -f deploymentservice-rg.yaml
    ```

3. **Verify the resources**: Check the status of the resources created by the
    ResourceGroup using the `kubectl` command:
    
    ```bash
    kubectl get rg
    ```

    You should see something like this:

    ```bash
    NAME                                   APIVERSION   KIND                STATE    AGE
    deploymentservice.x.symphony.k8s.aws   v1alpha1     DeploymentService   ACTIVE   16m
    ```

4. **Install a resource group claim**: Create a claim for the resource group
    you just deployed. Claims are used to define the desired state of the
    resources in the ResourceGroup.

    Here's an example of a Claim for the `EKSCluster` ResourceGroup:

    ```yaml
    apiVersion: x.symphony.k8s.aws/v1alpha1
    kind: DeploymentService
    metadata:
      name: my-deployment-and-service
    spec:
      name: app1
    ```

    The spec fields of a Claim correspond to the parameters defined in the
    ResourceGroup.