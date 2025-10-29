---
sidebar_position: 306
---

# Pod with RDS DBInstance

```yaml title="deploymentdbinstance-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: deploymentandawspostgres
spec:
  # CRD Definition
  schema:
    apiVersion: v1alpha1
    kind: DeploymentAndAWSPostgres
    spec:
      applicationName: string
      image: string
      location: string

  # Resources
  resources:
    - id: dbinstance
      template:
        apiVersion: rds.services.k8s.aws/v1alpha1
        kind: DBInstance
        metadata:
          name: ${schema.spec.applicationName}-dbinstance
        spec:
          # need to specify the required fields (e.g masterUsername, masterPassword)
          engine: postgres
          dbInstanceIdentifier: ${schema.spec.applicationName}-dbinstance
          allocatedStorage: 20
          dbInstanceClass: db.t3.micro

    - id: pod
      template:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${schema.spec.applicationName}-pod
        spec:
          containers:
            - name: container1
              image: ${schema.spec.image}
              env:
                - name: POSTGRESS_ENDPOINT
                  value: ${dbinstance.status.endpoint.address}
```
