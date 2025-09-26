---
sidebar_label: "FAQ"
sidebar_position: 100
---

# FAQ

1. **What is kro?**

   Kube Resource Orchestrator (**kro**) is a new operator for Kubernetes that
   simplifies the creation of complex Kubernetes resource configurations. kro
   lets you create and manage custom groups of Kubernetes resources by defining
   them as a _ResourceGraphDefinition_, the project's fundamental custom resource.
   ResourceGraphDefinition specifications define a set of resources and how they relate to
   each other functionally. Once defined, resource groups can be applied to a
   Kubernetes cluster where the kro controller is running. Once validated by
   kro, you can create instances of your resource group. kro translates your
   ResourceGraphDefinition instance and its parameters into specific Kubernetes resources
   and configurations which it then manages for you.

2. **How does kro work?**

   kro is designed to use core Kubernetes primitives to make resource grouping,
   customization, and dependency management simpler. When a ResourceGraphDefinition is
   applied to the cluster, the kro controller verifies its specification, then
   dynamically creates a new CRD and registers it with the API server. kro then
   deploys a dedicated controller to respond to instance events on the CRD. This
   microcontroller is responsible for managing the lifecycle of resources
   defined in the ResourceGraphDefinition for each instance that is created.

3. **How do I use kro?**

   First, you define your custom resource groups by creating _ResourceGraphDefinition_
   specifications. These specify one or more Kubernetes resources, and can
   include specific configuration for each resource.

   For example, you can define a _WebApp_ resource group that is composed of a
   _Deployment_, pre-configured to deploy your web server backend, and a
   _Service_ configured to run on a specific port. You can just as easily create
   a more complex _WebAppWithDB_ resource group by combining the existing
   _WebApp_ resource group with a _Table_ custom resource to provision a cloud
   managed database instance for your web app to use.

   Once you have defined a ResourceGraphDefinition, you can apply it to a Kubernetes
   cluster where the kro controller is running. kro will take care of the heavy
   lifting of creating CRDs and deploying dedicated controllers in order to
   manage instances of your new custom resource group.

   You are now ready to create instances of your new custom resource group, and
   kro will respond by dynamically creating, configuring, and managing the
   underlying Kubernetes resources for you.

4. **Why did you build this project?**

   We want to help streamline and simplify building with Kubernetes. Building
   with Kubernetes means dealing with resources that need to operate and work
   together, and orchestrating this can get complex and difficult at scale. With
   this project, we're taking a first step in reducing the complexity of
   resource dependency management and customization, paving the way for a simple
   and scalable way to create complex custom resources for Kubernetes.

5. **Can I use this in production?**

   This project is in active development and not yet intended for production
   use. The _ResourceGraphDefinition_ CRD and other APIs used in this project are not
   solidified and highly subject to change.


6. **What are the current known issues and limitations?**

   This section documents the most frequently observed and problematic issues reported by the kro community.

    **Current Known Issues**

   **1. Missing API upgrade support for RGD-based CRDs**

   - **Impact**: When Platform teams modify ResourceGraphDefinition schemas, there's no migration path for existing instances created by Developer teams
   - **Status**: Major architectural limitation blocking production adoption  
   - **Workaround**: Manual recreation of all instances when RGD schemas change
   - **Community Impact**: Most frequently reported limitation preventing enterprise use

   **2. Resource creation failures in complex dependency chains**

   - **Impact**: Some resources may not be created when ResourceGraphDefinitions have complex dependencies, particularly with topological sorting issues
   - **Status**: Confirmed issue affecting multiple users
   - **Workaround**: Simplify resource dependencies or manually verify all resources are created
   - **GitHub Issue**: [#225](https://github.com/kro-run/kro/issues/225)

   **3. GitOps integration compatibility**

   - **Impact**: Wrong API endpoints used with Fleet and other GitOps tools causing integration failures
   - **Status**: Confirmed integration problem affecting GitOps workflows
   - **Workaround**: Manual intervention or alternative deployment strategies  
   - **GitHub Issue**: [#524](https://github.com/kro-run/kro/issues/524)


   **Current Limitations**

   **1. Production readiness**
   - **Limitation**: Project is in active development with no guarantee of backward compatibility between versions
   - **Impact**: ResourceGraphDefinition CRDs and APIs are subject to breaking changes, and upgrades may require manual migration of existing ResourceGraphDefinitions
   - **Recommendation**: Use only for development and testing environments

   **2. CEL expression observability**
   - **Limitation**: No visibility into CEL expression execution costs, evaluation performance, or debugging information
   - **Impact**: Difficult to troubleshoot and optimize complex ResourceGraphDefinitions when CEL expressions are expensive or fail
   - **Status**: Architectural gap in observability framework
   - **GitHub Issue**: [#190](https://github.com/kro-run/kro/issues/190)

   **How to Report Issues**

   For bug reports and feature requests, please follow our [contributing guidelines](https://github.com/kro-run/kro/blob/main/CONTRIBUTING.md#reporting-bugsfeature-requests).

   Note - This section focuses on the most frequently observed problematic issues confirmed by the community. 
   Updated during majro releases based on community feedback. 

 