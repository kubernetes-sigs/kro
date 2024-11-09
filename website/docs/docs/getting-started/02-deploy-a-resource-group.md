---
sidebar_position: 2
---

# Deploy a Resource Group

ResourceGroups are the core building blocks of KRO. They define the structure of
your application and the resources it requires. In this guide, you'll learn how
to deploy a ResourceGroup using KRO.

## Prerequisites

Before you begin, ensure you have the following:

- Installed KRO on your Kubernetes cluster
- A ResourceGroup manifest file

For this examole, we'll use a simple ResourceGroup that defines a Deployment and
a Service. Here's an example of a `ResourceGroup` manifest file:

```yaml title="deploymentservice-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
metadata:
  name: deployment-service
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
          name: ${schema.spec.name}
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
                - name: ${schema.spec.name}-deployment
                  image: nginx
                  ports:
                    - containerPort: 80
    - name: service
      definition:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
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
   deployment-service                     v1alpha1     DeploymentService   ACTIVE   16m
   ```

4. **Install a resource group instance**: Create an instance for the resource group you
   just deployed. Instances are used to define the desired state of the resources
   in the ResourceGroup.

   Here's an example of an Instance for the `EKSCluster` ResourceGroup:

   ```yaml
   apiVersion: kro.run/v1alpha1
   kind: DeploymentService
   metadata:
     name: my-deployment-and-service
   spec:
     name: app1
   ```

   The spec fields of an Instance correspond to the parameters defined in the
   ResourceGroup.
