---
sidebar_position: 2
---

# Quick Start

Create your first ResourceGraphDefinition and deploy an application.

## What is a **ResourceGraphDefinition (RGD)**?

A `ResourceGraphDefinition` (RGD) lets you create new Kubernetes APIs that deploy multiple
resources together as a single, reusable unit. In this example, we'll create an
RGD that packages a reusable set of resources, including a
`Deployment`, `Service`, and `Ingress`. These resources are available in any
Kubernetes cluster. Users can then call the API to deploy resources as a single
unit, ensuring they're always created together with the right configuration.

Under the hood, when you create an RGD, kro:

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

## Create your Application RGD

Let's create an RGD that combines a `Deployment`, a `Service` and
`Ingress`. Save this as `resourcegraphdefinition.yaml`:

```kro title="resourcegraphdefinition.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-application
spec:
  # kro uses this simple schema to create your CRD schema and apply it
  # The schema defines what users can provide when they instantiate the RGD (create an instance).
  schema:
    apiVersion: kro.run/v1alpha1
    kind: Application
    spec:
      # Spec fields that users can provide.
      name: string
      image: string | default="nginx"
      replicas: integer | default=3
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
          replicas: ${schema.spec.replicas} # Use the replicas provided by user
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
          name: ${schema.spec.name}-svc
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

### Deploy the RGD

1. **Create an RGD manifest file**: Create a new file with the
   RGD definition. You can use the example above.

2. **Apply the RGD**: Use the `kubectl` command to deploy the
   RGD to your Kubernetes cluster:

   ```bash
   kubectl apply -f resourcegraphdefinition.yaml
   ```

3. **Inspect the RGD**: Check the status of the RGD using the `kubectl` command:

   ```bash
   kubectl get rgd my-application -owide
   ```

   You should see the RGD in the `Active` state, along with relevant
   information to help you understand your application:

   ```bash
   NAME             APIVERSION   KIND          STATE    TOPOLOGICALORDER                     AGE
   my-application   v1alpha1     Application   Active   ["deployment","service","ingress"]   49
   ```

### Create your Application Instance

Now that your RGD is created, kro has generated a new API
(Application) that orchestrates the creation of a `Deployment`, a `Service`, and
an `Ingress`. Let's use it!

1. **Create an Application instance**: Create a new file named `instance.yaml`
   with the following content:

   ```yaml title="instance.yaml"
   apiVersion: kro.run/v1alpha1
   kind: Application
   metadata:
     name: my-app-instance
   spec:
     name: my-app
     replicas: 1
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
   NAME              STATE    READY   AGE
   my-app-instance   Active   True    10s
   ```

   When you create an instance, kro automatically creates and manages all the underlying resources:

   <div style={{textAlign: 'center', marginTop: '40px', marginBottom: '40px'}}>

   ![Instance Resources](/img/instance-resources.svg)

   </div>

4. **Inspect the resources**: Check the resources created by the Application
   instance:

   ```bash
   kubectl get deployments,services,ingresses
   ```

   You should see the `Deployment`, `Service`, and `Ingress` created by the
   Application instance. Note the deployment has 1 replica as specified:

   ```bash
   NAME                      READY   UP-TO-DATE   AVAILABLE   AGE
   deployment.apps/my-app   1/1     1            1           69s

   NAME              TYPE        CLUSTER-IP      EXTERNAL-IP   PORT(S)   AGE
   service/my-app-svc   ClusterIP   10.100.167.72   <none>        80/TCP    65s

   NAME                                        CLASS    HOSTS   ADDRESS   PORTS   AGE
   ingress.networking.k8s.io/my-app-ingress   <none>   *                 80      62s
   ```

### Experiment with kro

kro continuously reconciles your resources. Try these experiments:

**Change the replica count:**

1. Edit `instance.yaml` to increase replicas to 3:

   ```yaml title="instance.yaml"
   apiVersion: kro.run/v1alpha1
   kind: Application
   metadata:
     name: my-app-instance
   spec:
     name: my-app
     replicas: 3
     ingress:
       enabled: true
   ```

2. Apply the change:

   ```bash
   kubectl apply -f instance.yaml
   ```

3. Watch the deployment scale up:

   ```bash
   kubectl get deployment my-app
   ```

**See kro's automatic reconciliation:**

1. Delete the service:

   ```bash
   kubectl delete service my-app-svc
   ```

2. Watch kro recreate it automatically:

   ```bash
   kubectl get service my-app-svc -w
   ```

   kro is watching and will instantly recreate the service to match your RGD definition.

### Clean Up

kro can also help you clean up resources when you're done with them.

1. **Delete the Application instance**: Clean up the resources by deleting the
   Application instance:

   ```bash
   kubectl delete application my-app-instance
   ```

   Now, the resources created by the Application instance will be deleted.
