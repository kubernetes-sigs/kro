---
sidebar_position: 20
---

# DeploymentDBInstance

```yaml title="deploymentdbinstance-rg.yaml"
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
metadata:
  name: deploymentandawspostgres
spec:
  apiVersion: v1alpha1
  kind: DeploymentAndAWSPostgres

  # CRD Definition
  definition:
    spec:
      applicationName: string
      image: string
      location: string

  # Resources
  resources:
    - name: dbinstance
      definition:
        apiVersion: rds.saervices.k8s.aws/v1alpha1
        kind: DBInstance
        metadata:
          name: ${spec.applicationName}-dbinstance
        spec:
          # need to specify the required fields (e.g masterUsername, masterPassword)
          engine: postgres
          dbInstanceIdentifier: ${spec.applicationName}-dbinstance
          allocatedStorage: 20
          dbInstanceClass: db.t3.micro

    - name: pod
      definition:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${spec.applicationName}-pod
        spec:
          containers:
            - name: container1
              image: ${spec.image}
              env:
                - name: POSTGRESS_ENDPOINT
                  value: ${dbinstance.status.endpoint.address}
```
