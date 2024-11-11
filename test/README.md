# Testing strategy for kro

This document outlines the testing strategy for **kro**, focusing on
**integration tests** and **end-to-end testing**. It defines the purpose,
approach and boundaries of each type of testing, providing clear guidelines for
contributors and maintainers.

kro is a complex controller that interacts with multiple Kubernetes resources
and depends on various Kubernetes features, such as custom resources, custom
controllers, informers, and client-go. The testing strategy aims to ensure that
kro works as expected in a Kubernetes environment, and that it can be safely
deployed in production clusters.

## Technical principles for testing

1. Use existing Kubernetes testing frameworks (when possible): Don't reinvent
   the wheel. If a feature is not covered by existing frameworks, contribute to
   them.
2. Focus on kro's logic, not on other controllers or Kubernetes components. e.g
   avoid testing native controllers, ACK or Karpenter's behaviour...
3. Prioritize integration tests, validate with end to end tests.
4. Maintain seperation of concerns, controller logic, integration tests, and e2e
   tests
5. Ensure readability: similar to the codebase, tests should be easy to read,
   understand and maintain.

## Directory structure

- `integration/`: Contains integration test suites for kro.
- `e2e/`: Contains e2e test suites for kro.
- `testdata/`: Directory for test data, such as Kubernetes manifests,
  resourcegroups ...

## Integration tests

In integration tests, the focus should be on your custom controller's logic and
its interactions with the Kubernetes API server. Instead of emulating other
controllers, you should:

1. Mock the Kubernetes API server responses
2. Verify that your controller makes the correct API calls
3. Test your controller's reconciliation logic
4. Check status updates on your custom resources

### Example testing ResourceGroups

1. Create a ResourceGroup
2. Trigger ResourceGroup reconciliation
3. Check that the ResourceGroup status was updated correctly
4. Verify that the correct Create (CRD) call was made to the API server
5. Check that the controller is watching the correct RG instances
6. Create a ResourceGroup instance
7. Trigger the ResourceGroup instance reconciliation
8. Check that the ResourceGroup instance status was updated correctly
9. Verify that the some resources were created in the cluster
10. Trigger a second reconciliation and check that the status was updated
    correctly
11. Repeat until all the RG instances are created
12. Do the same for updates and deletions

## E2e tests

E2E tests for kro should focus on validating the entire system's behavior in a
real Kubernetes environment. These tests ensure that kro works correctly with
actual Kubernetes resources and other controllers. The approach for E2E tests
should:

1. Use a real Kubernetes cluster (e.g. kind, minikube, or EKS)
2. Deploy kro controller and it's CRDs
3. Deploy other controllers or resources that will interact with kro resources.
4. Create kro ResourceGroups and ResourceGroupInstances and verify their full
   lifecycle.

### E2e test example

1. Deploy kro controller and CRDs
2. Deploy a sample application that uses kro
3. Create a `ResourceGroup` custom resource
4. Verify that the corresponding CRD is created in the cluster
5. Create an instance of the `ResourceGroup`
6. Wait for all resources defined in the `ResourceGroup` to be created
7. Verify that all resources are correctly created and configured
8. Update the `ResourceGroup` instance
9. Verify that the changes are correctly propagated to the managed resources
10. Delete the `ResourceGroup` instance
11. Verify that all managed resources are correctly deleted
12. Delete the `ResourceGroup` custom resource
13. Verify that the corresponding CRD is removed from the cluster

### Addional scenarios

1. Cross namespace resource management
2. Scaling testing: Create a large number of ResourceGroups and ResourceGroup
   instances to test kro at scale.
3. Failure recovery: Simulate failures in the controller or the Kubernetes API
   server and verify that kro recovers correctly
4. Controller upgrade testing: Deploy a new version of kro and verify that it
   can handle existing `ResourceGroups` and `ResourceGroup` instances
5. ResourceGroup conflict testing: Create multiple `ResourceGroups` with
   conflicting resources and verify that kro handles the conflicts correctly
6. Integration with other controllers: Deploy other controllers that interact
   with kro resources and verify that they work correctly together
