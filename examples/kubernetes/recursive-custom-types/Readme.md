# kro recursive custom type reference example

This example creates a ResourceGraphDefinition called `CompanyConfig` with three custom types referencing mong each other

### Create ResourceGraphDefinition called CompanyConfig

Apply the RGD to your cluster:

```
kubectl apply -f rg.yaml
```

Validate the RGD status is Active:

```
kubectl get rgd company.kro.run
```

Expected result:

```
NAME              APIVERSION   KIND            STATE    AGE
company.kro.run   v1alpha1     CompanyConfig   Active   XXX
```

### Create an Instance of kind App

Apply the provided instance.yaml

```
kubectl apply -f instance.yaml
```

Validate instance status:

```
kubectl get companyconfigs my-company-config
```

Expected result:

```
NAME                STATE    SYNCED   AGE
my-company-config   ACTIVE   True     XXX
```

### Clean up

Remove the instance:

```
kubectl delete companyconfigs my-company-config
```

Remove the resourcegraphdefinition:

```
kubectl delete rgd company.kro.run
```
