---
sidebar_position: 2
---

# Deploy Your First ResourceGraphDefinition

This guide will walk you through creating your first Resource Graph Definition in **kro**.
We'll create a simple `ResourceGraphDefinition` that demonstrates key kro features.

## What is a **ResourceGraphDefinition**?

A `ResourceGraphDefinition` lets you create new Kubernetes APIs that deploy multiple
resources together as a single, reusable unit. In this example, we’ll create a
`ResourceGraphDefinition` that packages a reusable set of resources, including a
`Deployment`, `Service`, and `Ingress`. These resources are available in any
Kubernetes cluster. Users can then call the API to deploy resources as a single
unit, ensuring they're always created together with the right configuration.

Under the hood, when you create a `ResourceGraphDefinition`, kro:

1. Treats your resources as a Directed Acyclic Graph (DAG) to understand their
   dependencies
2. Validates resource definitions and detects the correct deployment order
3. Creates a new API (CRD) in your cluster
4. Configures itself to watch and serve instances of this API

## Prerequisites

Before you begin, make sure you have the following:

- **kro** [installed](./01-Installation.md) and running in your Kubernetes
  cluster.
- `kubectl` installed and configured to interact with your Kubernetes cluster.

## Create your Application ResourceGraphDefinition

Let's create a Resource Graph Definition that combines a `Deployment`, a `Service` and
`Ingress`. Save this as `resourcegraphdefinition.yaml`:

```yaml title="resourcegraphdefinition.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-application
spec:
  # kro uses this simple schema to create your CRD schema and apply it
  # The schema defines what users can provide when they instantiate the RG (create an instance).
  schema:
    apiVersion: v1alpha1
    kind: Application
    spec:
      # Spec fields that users can provide.
      name: string
      image: string | default="nginx"
      ingress:
        enabled: boolean | default=false
    status:
      # Fields the controller will inject into instances status.
      deploymentConditions: ${deployment.status.conditions}
      availableReplicas: ${deployment.status.availableReplicas}

  # Define the resources this API will manage.
  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name} # Use the name provided by user
        spec:
          replicas: 3
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: ${schema.spec.name}
                  image: ${schema.spec.image} # Use the image provided by user
                  ports:
                    - containerPort: 80

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}-service
        spec:
          selector: ${deployment.spec.selector.matchLabels} # Use the deployment selector
          ports:
            - protocol: TCP
              port: 80
              targetPort: 80

    - id: ingress
      includeWhen:
        - ${schema.spec.ingress.enabled} # Only include if the user wants to create an Ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: ${schema.spec.name}-ingress
          annotations:
            kubernetes.io/ingress.class: alb
            alb.ingress.kubernetes.io/scheme: internet-facing
            alb.ingress.kubernetes.io/target-type: ip
            alb.ingress.kubernetes.io/healthcheck-path: /health
            alb.ingress.kubernetes.io/listen-ports: '[{"HTTP": 80}]'
            alb.ingress.kubernetes.io/target-group-attributes: stickiness.enabled=true,stickiness.lb_cookie.duration_seconds=60
        spec:
          rules:
            - http:
                paths:
                  - path: "/"
                    pathType: Prefix
                    backend:
                      service:
                        name: ${service.metadata.name} # Use the service name
                        port:
                          number: 80
```

### Deploy the ResourceGraphDefinition

1. **Create a ResourceGraphDefinition manifest file**: Create a new file with the
   `ResourceGraphDefinition` definition. You can use the example above.

2. **Apply the `ResourceGraphDefinition`**: Use the `kubectl` command to deploy the
   ResourceGraphDefinition to your Kubernetes cluster:

   ```bash
   kubectl apply -f resourcegraphdefinition.yaml
   ```

3. **Inpsect the `ResourceGraphDefinition`**: Check the status of the resources created by
   the ResourceGraphDefinition using the `kubectl` command:

   ```bash
   kubectl get rg my-application -owide
   ```

   You should see the ResourceGraphDefinition in the `Active` state, along with relevant
   information to help you understand your application:

   ```bash
   NAME             APIVERSION   KIND          STATE    TOPOLOGICALORDER                     AGE
   my-application   v1alpha1     Application   Active   ["deployment","service","ingress"]   49
   ```

### Create your Application Instance

Now that your `ResourceGraphDefinition` is created, kro has generated a new API
(Application) that orchestrates creation of the a `Deployment`, a `Service`, and
an `Ingress`. Let's use it!

1. **Create an Application instance**: Create a new file named `instance.yaml`
   with the following content:

   ```yaml title="instance.yaml"
   apiVersion: kro.run/v1alpha1
   kind: Application
   metadata:
     name: my-application-instance
   spec:
     name: my-awesome-app
     ingress:
       enabled: true
   ```

2. **Apply the Application instance**: Use the `kubectl` command to deploy the
   Application instance to your Kubernetes cluster:

   ```bash
   kubectl apply -f instance.yaml
   ```

3. **Inspect the Application instance**: Check the status of the resources

   ```bash
   kubectl get applications
   ```

   After a few seconds, you should see the Application instance in the `Active`
   state:

   ```bash
   NAME                      STATE    SYNCED   AGE
   my-application-instance   ACTIVE   True     10s
   ```

4. **Inspect the resources**: Check the resources created by the Application
   instance:

   ```bash
   kubectl get deployments,services,ingresses
   ```

   You should see the `Deployment`, `Service`, and `Ingress` created by the
   Application instance.

   ```bash
   NAME                             READY   UP-TO-DATE   AVAILABLE   AGE
   deployment.apps/my-awesome-app   3/3     3            3           69s

   NAME                             TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
   service/my-awesome-app-service   ClusterIP   10.100.167.72   <none>        80/TCP    65s

   NAME                                               CLASS    HOSTS   ADDRESS   PORTS   AGE
   ingress.networking.k8s.io/my-awesome-app-ingress   <none>   *                 80      62s
   ```

### Delete the Application instance

kro can also help you clean up resources when you're done with them.

1. **Delete the Application instance**: Clean up the resources by deleting the
   Application instance:

   ```bash
   kubectl delete application my-application-instance
   ```

   Now, the resources created by the Application instance will be deleted.
