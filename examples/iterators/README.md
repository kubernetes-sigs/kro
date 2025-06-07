# Looping through list values

This example demonstrates the iterators feature to generate repeated YAML
fragments from a list. The `resourceGraphDefinition.loop.yaml` defines an iterator
named `deploymentContainerPorts` which renders container port blocks for each
entry in `schema.spec.ports`.

Apply the ResourceGraphDefinition and then create the `loop.dev.yaml`
instance. The `includeWhen` expression filters ports so only those with
`ingress: true` are rendered in the Deployment.

Files:
- `resourceGraphDefinition.loop.yaml` – ResourceGraphDefinition that create a Loop CR with iterators
- `loop.dev.yaml` – example instance
- `deployment.result.yaml` – expected Deployment output
