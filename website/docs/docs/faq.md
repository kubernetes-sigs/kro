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

   This section documents frequently encountered issues and limitations reported by the kro community.

   ### Current Known Issues

   **1. Kro does not detect deleted resources**

   - **Impact**: Deleted resources from kro instances are not detected and recreated automatically
   - **Status**: Open bug
   - **Workaround**: Manual recreation of deleted resources required
   - **GitHub Issue**: [#520](https://github.com/kro-run/kro/issues/520)

   **2. RGD instances stuck in "IN_PROGRESS" status**

   - **Impact**: ResourceGraphDefinition instances remain in IN_PROGRESS status indefinitely
   - **Status**: Under investigation
   - **Workaround**: Manual intervention may be required
   - **GitHub Issue**: [#407](https://github.com/kro-run/kro/issues/407)

   **3. Missing detection of CRD breaking changes**

   - **Impact**: Kro does not detect breaking changes in ResourceGroup schemas when modified
   - **Status**: Feature enhancement needed
   - **Workaround**: Manual validation of schema changes required
   - **GitHub Issue**: [#186](https://github.com/kro-run/kro/issues/186)

   **4. SNS topic creation failures**

   - **Impact**: Cannot create SNS topics through kro despite correct IAM permissions
   - **Status**: Open bug
   - **Workaround**: Create topics manually or use manifest files directly
   - **GitHub Issue**: [#522](https://github.com/kro-run/kro/issues/522)

 

   ## Current Limitations

   **1. Production readiness**
   - **Limitation**: Project is in active development and not intended for production use
   - **Impact**: ResourceGraphDefinition CRD and APIs are not solidified and subject to change
   - **Recommendation**: Use only for development and testing environments

   **2. API stability**
   - **Limitation**: No guarantee of backward compatibility between versions
   - **Impact**: Upgrades may require manual migration of existing ResourceGraphDefinitions
   - **Recommendation**: Plan for potential breaking changes during upgrades

   **3. Resource dependency complexity**
   - **Limitation**: Complex dependency chains may be difficult to troubleshoot
   - **Impact**: Issues in deeply nested resource dependencies can be hard to diagnose
   - **Recommendation**: Keep ResourceGraphDefinition designs as simple as possible



   **How to Contribute**

   This section is community-maintained:

   - Found a recurring issue? Check if it's already tracked in our [GitHub Issues](https://github.com/kro-run/kro/issues)
   - For widespread issues affecting multiple users, consider updating this page via pull request
   - Issues discussed in community calls will be added here




   **Stay Updated**

   - **Community Calls**: Join our [bi-weekly community meetings](https://docs.google.com/document/d/1GqeHcBlOw6ozo-qS4TLdXSi5qUn88QU6dwdq0GvxRz4/edit)
   - **Live Issues**: [GitHub Issues](https://github.com/kro-run/kro/issues)
   - **Community Slack**: [#kro channel](https://kubernetes.slack.com/archives/C081TMY9D6Y) in Kubernetes Slack

   *Last updated: August 16, 2025 | This section is updated during major releases*