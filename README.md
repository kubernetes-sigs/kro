# kro | Kube Resource Orchestrator

This project aims to simplify the creation and management of complex custom resources for Kubernetes.

Kube Resource Orchestrator (**kro**) provides a powerful abstraction layer that allows you to define complex multi-resource constructs as reusable components in your applications and systems.
You define these using kro's fundamental custom resource, *ResourceGraphDefinition*.
This resource serves as a blueprint for creating and managing collections of underlying Kubernetes resources.

With kro, you define custom resources as your fundamental building blocks for Kubernetes.
These building blocks can include other Kubernetes resources, either native or custom, and can specify the dependencies between them.
This lets you define complex custom resources, and include default configurations for their use.
The kro controller will determine the dependencies between resources, establish the correct order of operations to create and configure them, and then dynamically create and manage all of the underlying resources for you.

kro is Kubernetes native and integrates seamlessly with existing tools to preserve familiar processes and interfaces.

## Documentation

| Title                                  | Description                     |
| -------------------------------------- | ------------------------------- |
| [Introduction][kro-overview]           | An introduction to kro          |
| [Installation][kro-installation]       | Install kro in your cluster     |
| [Getting started][kro-getting-started] | Deploy your first ResourceGraphDefinition |
| [Concepts][kro-concepts]               | Learn more about kro concepts   |
| [Examples][kro-examples]               | Example resources               |
| [Contributions](./CONTRIBUTING.md)       | How to get involved             |

[kro-overview]: https://kro.run/docs/overview
[kro-installation]: https://kro.run/docs/getting-started/Installation
[kro-getting-started]: https://kro.run/docs/getting-started/deploy-a-resource-group
[kro-concepts]: https://kro.run/docs/concepts/resource-groups/
[kro-examples]: https://kro.run/examples/

## FAQ

1. **What is kro?**

    Kube Resource Orchestrator (**kro**) is a new operator for Kubernetes that simplifies the creation of complex Kubernetes resource configurations.
    kro lets you create and manage custom groups of Kubernetes resources by defining them as a *ResourceGraphDefinition*, the project's fundamental custom resource.
    ResourceGraphDefinition specifications define a set of resources and how they relate to each other functionally.
    Once defined, resource groups can be applied to a Kubernetes cluster where the kro controller is running.
    Once validated by kro, you can create instances of your resource group.
    kro translates your ResourceGraphDefinition instance and its parameters into specific Kubernetes resources and configurations which it then manages for you.

2. **How does kro work?**

    kro is designed to use core Kubernetes primitives to make resource grouping, customization, and dependency management simpler.
    When a ResourceGraphDefinition is applied to the cluster, the kro controller verifies its specification, then dynamically creates a new CRD and registers it with the API server.
    kro then deploys a dedicated controller to respond to instance events on the CRD.
    This microcontroller is responsible for managing the lifecycle of resources defined in the ResourceGraphDefinition for each instance that is created.

3. **How do I use kro?**

    First, you define your custom resource groups by creating *ResourceGraphDefinition* specifications.
    These specify one or more Kubernetes resources, and can include specific configuration for each resource.

    For example, you can define a *WebApp* resource group that is composed of a *Deployment*, pre-configured to deploy your web server backend, and a *Service* configured to run on a specific port.
    You can just as easily create a more complex *WebAppWithDB* resource group by combining the existing *WebApp* resource group with a *Table* custom resource to provision a cloud managed database instance for your web app to use.

    Once you have defined a ResourceGraphDefinition, you can apply it to a Kubernetes cluster where the kro controller is running.
    kro will take care of the heavy lifting of creating CRDs and deploying dedicated controllers in order to manage instances of your new custom resource group.

    You are now ready to create instances of your new custom resource group, and kro will respond by dynamically creating, configuring, and managing the underlying Kubernetes resources for you.

4. **Why did you build this project?**

    We want to help streamline and simplify building with Kubernetes.
    Building with Kubernetes means dealing with resources that need to operate and work together, and orchestrating this can get complex and difficult at scale.
    With this project, we're taking a first step in reducing the complexity of resource dependency management and customization, paving the way for a simple and scalable way to create complex custom resources for Kubernetes.

5. **Can I use this in production?**

   This project is in active development and not yet intended for production use.
   The *ResourceGraphDefinition* CRD and other APIs used in this project are not solidified and highly subject to change.

## Community Participation

Development and discussion is coordinated in the [Kubernetes Slack (invite link)][k8s-slack] channel [#kro][kro-channel] channel.

[k8s-slack]: https://communityinviter.com/apps/kubernetes/community
[kro-channel]: https://kubernetes.slack.com/archives/C081TMY9D6Y

## Security

See [CONTRIBUTING](CONTRIBUTING.md#security-issue-notifications) for more information.

## License

[Apache 2.0](LICENSE)
