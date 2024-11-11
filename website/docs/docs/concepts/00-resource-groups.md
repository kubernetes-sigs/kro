---
sidebar_position: 1
---

# ResourceGroups

**ResourceGroups** are the fundamental building blocks in KRO. They provide a
way to define, organize, and manage sets of related Kubernetes resources as a
single, reusable unit.

## What is a **ResourceGroup**?

A **ResourceGroup** is a custom resource that serves as a blueprint for creating
and managing a collection of Kubernetes resources. It allows you to:

- Define multiple resources in a single, cohesive unit
- Establish relationships and dependencies between resources
- Create high-level abstractions of complex Kubernetes configurations
- Promote reusability and consistency across your infrastructure

## Anatomy of a **ResourceGroup**

A **ResourceGroup**, like any Kubernetes resource, consists of three main parts:

1. **Metadata**: Name, namespace, labels, etc.
2. **Spec**: Defines the structure and properties of the ResourceGroup
3. **Status**: Reflects the current state of the ResourceGroup

The `spec` section of a ResourceGroup typically includes:

- **Parameters**: Define the customizable aspects of the ResourceGroup
- **Resources**: Specify the Kubernetes resources to be created
- The **kind** and **apiVersion** fields within the spec define the CRD that
  will be generated for this ResourceGroup. Here's a simple example of a
  ResourceGroup:

```yaml text title="simple-web-app.yaml"
apiVersion: kro.run/v1
kind: ResourceGroup
metadata:
  name: simple-web-app
spec:
  kind: SimpleWebApp
  apiVersion: v1alpha1
  parameters:
    appName: string
    image: string
    replicas: int
  resources:
    - name: deployment
      definition:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.appName}-deployment
        spec:
          replicas: ${schema.spec.replicas}
          selector:
            matchLabels:
              app: ${schema.spec.appName}
          template:
            metadata:
              labels:
                app: ${schema.spec.appName}
            spec:
              containers:
                - name: ${schema.spec.appName}-container
                  image: ${schema.spec.image}
    - name: service
      definition:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.appName}-service
        spec:
          selector:
            app: ${schema.spec.appName}
          ports:
            - port: 80
              targetPort: 80
```

In this example, the **ResourceGroup** defines a simple web application with a
Deployment and a Service. The appName, image, and replicas are parameters that
can be set when instantiating this ResourceGroup.

## **ResourceGroup** Processing

When a **ResourceGroup** is submitted to the Kubernetes API server, the KRO
controller processes it as follows:

1. **Formal Verification**: The controller performs a comprehensive analysis of
   the ResourceGroup definition. This includes:

   - **Syntax checking**: Ensuring all fields are correctly formatted.
   - **Type checking**: Validating that parameter types match their definitions.
   - **Semantic validation**: Verifying that resource relationships and
     dependencies are logically sound.
   - **Dry-run validation**: Simulating the creation of resources to detect
     potential issues.

2. **CRD Generation**: The controller automatically generates a new **Custom
   Resource Definition (CRD)** based on the ResourceGroup's specification. This
   CRD represents the type for instances of this ResourceGroup.

3. **CRD Registration**: It registers the newly generated CRD with the
   Kubernetes API server, making it available for use in the cluster.

4. **Micro-Controller Deployment**: KRO deploys a dedicated micro-controller for
   this ResourceGroup. This micro-controller will listen for **"instance"
   events** - instances of the CRD created in step 2. It will be responsible for
   managing the **lifecycle of resources** defined in the ResourceGroup for each
   instance.

5. **Status Update**: The controller updates the status of the ResourceGroup to
   reflect that the corresponding CRD has been created and registered.

For example, given our `simple-web-app` ResourceGroup, the controller would
create and register a CRD named `SimpleWebApps` (plural form of the
ResourceGroup name). This CRD defines the structure for creating instances of
the web application with customizable parameters. The deployed micro-controller
would then manage all **SimpleWebApps instances**, creating and managing the
associated **Deployments** and **Services** as defined in the ResourceGroup.

The **KRO** controller continues to monitor the **ResourceGroup** for any
changes, updating the corresponding CRD and micro-controller as necessary.

## **ResourceGroup** Instance Example

After the **ResourceGroup** is processed, users can create instances of it.
Here's an example of how an instance for the `SimpleWebApp` might look:

```yaml title="my-web-app-instance.yaml"
apiVersion: kro.run/v1alpha1
kind: SimpleWebApp
metadata:
  name: my-web-app
spec:
  appName: awesome-app
  image: nginx:latest
  replicas: 3
```

In the next section, we'll explore the `parameters` and `resources` sections of
a **ResourceGroup** in more detail.
