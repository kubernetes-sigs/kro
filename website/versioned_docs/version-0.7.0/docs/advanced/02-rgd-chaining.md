---
sidebar_position: 2
---

# RGD Chaining

RGD chaining allows you to compose complex applications by building on top of existing ResourceGraphDefinitions. Instead of duplicating resource definitions, you can create instances of one RGD within another RGD's resource graph.

## How It Works

When you create an RGD, kro registers a new Custom Resource Definition (CRD) in your cluster. This CRD can then be used as a resource in other RGDs, just like any other Kubernetes resource.

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';

<Tabs>
<TabItem value="fullstack" label="FullStackApp (Parent)" default>

The parent RGD uses instances of other RGDs as resources:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: full-stack-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: FullStackApp
    spec:
      name: string
      dbSize: string | default="small"
    status:
      databaseReady: ${database.status.ready}
      appEndpoint: ${webapp.status.endpoint}

  resources:
    # Use an instance of the Database RGD
    - id: database
      template:
        apiVersion: kro.run/v1alpha1
        kind: Database
        metadata:
          name: ${schema.spec.name}-db
        spec:
          size: ${schema.spec.dbSize}
      readyWhen:
        - ${database.status.ready == true}

    # Use an instance of the WebApplication RGD
    - id: webapp
      template:
        apiVersion: kro.run/v1alpha1
        kind: WebApplication
        metadata:
          name: ${schema.spec.name}-web
        spec:
          name: ${schema.spec.name}
          image: myapp:latest
          databaseUrl: ${database.status.connectionString}
```

</TabItem>
<TabItem value="database" label="Database RGD">

The Database RGD creates a StatefulSet and exposes connection info:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: database
spec:
  schema:
    apiVersion: v1alpha1
    kind: Database
    spec:
      size: string | default="small"
    status:
      ready: ${statefulset.status.readyReplicas == 1}
      connectionString: "postgresql://postgres@${service.metadata.name}:5432/postgres"

  resources:
    - id: secret
      template:
        apiVersion: v1
        kind: Secret
        metadata:
          name: ${schema.metadata.name}-credentials
        stringData:
          password: changeme

    - id: statefulset
      template:
        apiVersion: apps/v1
        kind: StatefulSet
        metadata:
          name: ${schema.metadata.name}
        spec:
          serviceName: ${schema.metadata.name}
          replicas: 1
          selector:
            matchLabels:
              app: ${schema.metadata.name}
          template:
            metadata:
              labels:
                app: ${schema.metadata.name}
            spec:
              containers:
                - name: postgres
                  image: postgres:15
                  env:
                    - name: POSTGRES_PASSWORD
                      valueFrom:
                        secretKeyRef:
                          name: ${secret.metadata.name}
                          key: password
      readyWhen:
        - ${statefulset.status.readyReplicas == 1}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.metadata.name}
        spec:
          selector:
            app: ${schema.metadata.name}
          ports:
            - port: 5432
```

</TabItem>
<TabItem value="webapp" label="WebApplication RGD">

The WebApplication RGD creates a Deployment, Service, and Ingress:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: web-application
spec:
  schema:
    apiVersion: v1alpha1
    kind: WebApplication
    spec:
      name: string
      image: string
      databaseUrl: string | default=""
    status:
      ready: ${deployment.status.availableReplicas == 2}
      endpoint: "https://${schema.spec.name}.example.com"

  resources:
    - id: deployment
      template:
        apiVersion: apps/v1
        kind: Deployment
        metadata:
          name: ${schema.spec.name}
        spec:
          replicas: 2
          selector:
            matchLabels:
              app: ${schema.spec.name}
          template:
            metadata:
              labels:
                app: ${schema.spec.name}
            spec:
              containers:
                - name: app
                  image: ${schema.spec.image}
                  env:
                    - name: DATABASE_URL
                      value: ${schema.spec.databaseUrl}
                  ports:
                    - containerPort: 8080
      readyWhen:
        - ${deployment.status.availableReplicas == 2}

    - id: service
      template:
        apiVersion: v1
        kind: Service
        metadata:
          name: ${schema.spec.name}
        spec:
          selector:
            app: ${schema.spec.name}
          ports:
            - port: 80
              targetPort: 8080

    - id: ingress
      template:
        apiVersion: networking.k8s.io/v1
        kind: Ingress
        metadata:
          name: ${schema.spec.name}
        spec:
          rules:
            - host: ${schema.spec.name}.example.com
              http:
                paths:
                  - path: /
                    pathType: Prefix
                    backend:
                      service:
                        name: ${service.metadata.name}
                        port:
                          number: 80
```

</TabItem>
</Tabs>

## Building Blocks

RGD chaining enables a building blocks approach to platform engineering. Instead of creating monolithic RGDs that define everything, you can decompose your platform into small, focused RGDs that each do one thing well.

Lower-level RGDs expose their capabilities through status fields - connection strings, endpoints, credentials, ready states. Higher-level RGDs consume these outputs as inputs, wiring components together without needing to understand their internals.

This model lets platform teams build progressively higher abstractions. Application developers interact with simple, high-level APIs while the underlying complexity is managed by the RGD hierarchy.

## Dependency Resolution

When chaining RGDs, kro automatically:

1. **Tracks dependencies** - Expressions like `${database.status.connectionString}` create dependencies between the outer RGD and the inner instance
2. **Waits for readiness** - The outer RGD waits for the inner instance's `Ready` condition before proceeding
3. **Propagates status** - You can expose inner instance status fields in the outer RGD's status

## Considerations

### Ordering

RGD instances are created in topological order based on CEL expression dependencies, just like any other resource. The inner instance must be ready before dependent resources can reference its status.

### Deletion

When deleting an instance of a chained RGD:
- The outer instance is deleted first
- This triggers deletion of its managed resources, including inner RGD instances
- Inner instances then delete their own managed resources

### Debugging

Debugging chained RGDs requires checking multiple levels:
1. Check the outer instance's conditions
2. If `ResourcesReady` is false, identify which inner instance failed
3. Check the inner instance's conditions for the root cause

```bash
# Check outer instance
kubectl get fullstackapp my-app -o yaml

# Check inner instances
kubectl get database my-app-db -o yaml
kubectl get webapplication my-app-web -o yaml
```

## Best Practices

**Keep RGDs small and focused** - Create RGDs with clear inputs (spec) and outputs (status) that make them easy to chain.

**Expose necessary status** - Inner RGDs should expose status fields that outer RGDs need (connection strings, endpoints, etc.).

**Use meaningful names** - Name inner instances based on the outer instance's name to maintain traceability.

**Consider depth** - Deep chaining (RGDs within RGDs within RGDs) can make debugging harder. Keep nesting reasonable.
