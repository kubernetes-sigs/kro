# Use ResourceDescriptor for External Reference Scope Resolution

## Problem statement

When reconciling instances, the controller needs to interact with external references (resources not managed by the instance but referenced by it). To do this, it needs to know the GroupVersionResource (GVR) and the scope (namespaced or cluster-scoped) of the external resource.

Previously, the controller relied on `restMapper.RESTMapping` at runtime to resolve the GVK to GVR and determine the scope. This introduces a runtime dependency on the discovery client and can lead to errors if the REST mapper is not up-to-date or if the discovery fails. It also adds unnecessary latency to the reconciliation loop.

## Proposal

### Overview

We propose to use the `ResourceDescriptor` interface, which is populated during the ResourceGraphDefinition (RGD) compilation phase, to determine the GVR and scope of external references. The `ResourceDescriptor` already contains this information, as it is resolved when the RGD is processed.

### Design details

The `readExternalRef` function in `pkg/controller/instance/controller_reconcile.go` will be updated to:

1.  Retrieve the `ResourceDescriptor` for the given resource ID from the runtime.
2.  Use `descriptor.GetGroupVersionResource()` to get the GVR.
3.  Use `descriptor.IsNamespaced()` to determine if the resource is namespaced.
4.  Construct the dynamic client using this information.

```go
func (igr *instanceGraphReconciler) readExternalRef(ctx context.Context, resourceID string, resource *unstructured.Unstructured) (*unstructured.Unstructured, error) {
    descriptor := igr.runtime.ResourceDescriptor(resourceID)
    gvr := descriptor.GetGroupVersionResource()

    var dynResource dynamic.ResourceInterface
    if descriptor.IsNamespaced() {
        namespace := igr.getResourceNamespace(resourceID)
        dynResource = igr.client.Resource(gvr).Namespace(namespace)
    } else {
        dynResource = igr.client.Resource(gvr)
    }
    // ...
}
```

## Benefits

*   **Performance**: Removes the need for a REST mapper lookup during every reconciliation of an external reference.
*   **Reliability**: Relies on the static analysis performed during RGD compilation, which is consistent with how other resources are handled.
*   **Consistency**: Aligns the handling of external references with managed resources, which already use `ResourceDescriptor`.

## Tradeoffs

*   **Static Definition**: This assumes that the scope of a resource does not change between RGD compilation and runtime. In Kubernetes, changing the scope of a CRD is a breaking change and requires re-creation, so this assumption holds true for practical purposes.

## Scoping

### What is in scope for this proposal?

*   Modifying `pkg/controller/instance/controller_reconcile.go`.
*   Updating `readExternalRef` implementation.

### What is not in scope?

*   Changes to RGD compilation logic (the information is already there).
*   Changes to other parts of the controller.

## Testing strategy

### Test plan

*   **Unit Tests**: Existing unit tests for the instance controller should pass.
*   **Manual Verification**: Verify that external references are correctly resolved and read by the controller.
