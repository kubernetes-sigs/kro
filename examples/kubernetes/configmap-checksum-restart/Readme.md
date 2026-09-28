# kro ConfigMap Checksum Restart example

This example creates a ResourceGraphDefinition called `ConfigMapChecksumRestart`
that groups a `ConfigMap` and a `Deployment`, and restarts the Deployment's
pods whenever the ConfigMap's data changes. See
[Dependencies & Ordering](../../../website/docs/docs/concepts/expressions/03-dependencies-ordering.md#pattern-restart-on-configmapsecret-change)
for how it works.

### Create ResourceGraphDefinition called ConfigMapChecksumRestart

Apply the RGD to your cluster:

```
kubectl apply -f rg.yaml
```

Validate the RGD status is Active:

```
kubectl get rgd configmapchecksumrestarts.kro.run
```

Expected result:

```
NAME                                 APIVERSION   KIND                      STATE    AGE
configmapchecksumrestarts.kro.run    v1alpha1     ConfigMapChecksumRestart  Active   5s
```

### Create an Instance of kind ConfigMapChecksumRestart

Apply the provided instance.yaml

```
kubectl apply -f instance.yaml
```

Check the annotation kro computed on the pod template:

```
kubectl get deployment test-app -o jsonpath='{.spec.template.metadata.annotations.checksum/config}'
```

### Trigger a restart

Change `spec.config` in instance.yaml (for example `LOG_LEVEL: "debug"`) and re-apply:

```
kubectl apply -f instance.yaml
kubectl rollout status deployment/test-app
```

### Clean up

Remove the instance:

```
kubectl delete configmapchecksumrestarts test-app
```

Remove the resourcegraphdefinition:

```
kubectl delete rgd configmapchecksumrestarts.kro.run
```
