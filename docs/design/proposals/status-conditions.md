# KRO Status Conditions

## Problem statement

KRO reconciles ResourceGraphDefinitions (RGD) into a CustomResourceDefinition
(CRD) with some default status schema. This status schema is used when an
instance of the CRD is created and further reconciled by the KRO controller. The
user of the system can check on the status of their instance to see the health
and state of the underlying resource graph and dependencies as defined in the
toplevel RGD.

Kubernetes has conventions on how
[Status](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties)
(specifically Conditions) should be shaped but historically have not taken a
strong enough opinion to enforce consistency across the ecosystem.

The sig-cli library `kstatus` takes the opinion of a set of standard conditions
and the understanding that they adhere to “abnormal-true” style conditions.

Today the KRO project defaults status to have the shape defined in
[meta v1 condition](https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Condition),
and defaults to a single
[]`InstanceSynced`](https://github.com/kro-run/kro/blob/b9d1822983c4dead2f6dae594041dae550f1847a/pkg/controller/instance/controller_status.go#L78)
condition, but has no concept of a top-level condition. It then exposes the
state of this condition in
[custom columns](https://github.com/kro-run/kro/blob/b9d1822983c4dead2f6dae594041dae550f1847a/pkg/graph/crd/defaults.go#L68-L73).
The user is able to create and map their own custom conditions in the provided
schema inside of an instance of an RGD.

### Dissent

I (Scott) believe “abnormal-true” condition style is an anti-pattern. I also
believe all conditions do not necessarily mean they extend the API of the
resource. The conditions patterns used in
[Knative](https://github.com/knative/), [Tekton](https://github.com/tektoncd),
[Karpenter](https://github.com/kubernetes-sigs/karpenter), and many other
controllers follow a pattern of a single top level condition (Ready or
Succeeded). Then based on the controller reconciling a resource, there can be
one or more sub-conditions that explain the state of the resource as it relates
to the implementation of the controller.

## Proposal

The conditions defined in the schema of a CRD from an RGD should align with the
“single top-level condition” pattern. By not needing to know the implementation
details of the controller reconciling the resource and only looking at the
“top-level” condition, we enable a form of duck-typing on readiness that allows
the ecosystem to create safer interactions.

We will adhere to the advice from upstream
[Kubernetes API Conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties),
except we will strongly prefer normal-true (positive polarity) conditions.

Since a Resource Graph will continue to update the downstream resources based on
the RGD template, it will always have a Ready condition as its top-level
condition. Based on the implementation of the controller in KRO, we will have
sub-conditions that describe the major work of the Resource Graph reconciler.

#### Design details

We will leverage a Condition Manager implementation, similar to the
[status package in aws labs](https://github.com/awslabs/operatorpkg/tree/main/status),
or the
[conditions manager in the Knative apis package](https://github.com/knative/pkg/blob/b7bbf4be5dbd9a8b8b60e06094bd8930cf241357/apis/condition_set.go#L57).
For code ownership concerns, I would recommend we fork and modify either of
these implementations and house it in-project.

Personally I prefer the patterns in Knative (I helped create them) of decoupling
the conditions management into the API layer, or at minimum away from the
reconciler packaging. Conditions and their management is a different concern
from reconciliation and it confuses how much testing the controller needs to
perform. Knative’s pattern is to create helper methods to set and update
sub-conditions. These tend to be named Mark<Something> methods or
PropagateSomething methods. The reconciler is marking or propagating a fact to
the API layer and the helper method decides exactly what this changes in an
effort to isolate the testing and responsibilities of the reconciler.

In general, reconciliation logic can be grouped into large chunks of effort. We
do this naturally when writing the reconciliation logic by grouping
functionality into methods. It tends to be the best pattern to apply the same
grouping to a sub-condition via a Propagate* or Mark* method.

The condition manager is responsible for propagating the most important and most
recent change to the top-level condition.

Another side effect of using a condition manager is that sub-conditions become
implementation details and the only thing that is required for an observer to
observe is the top-level condition.

##### On Condition Status

There are three and only three values supported by a condition.Status: `True`,
`False`, and `Unknown`.

- True means the condition passes and there are no concerns for an operator. All
  should be well and working. In happy systems, things are all true.
- False means there is a fundamental issue that requires intervention. The
  reconciler is stopped from making progress until that issue is resolved. This
  could be as simple as an incorrect schema, or as complex as a downstream
  system only returning 500s and some on-call needs to be called.
- Unknown means the reconciler is making progress, but has not yet successfully
  fulfilled the request.

> Note: A False condition is nearly as serious as a fatal log line. It is not to
> be used lightly.

Resource Graph DefinitionThe Resource Graph Definition currently uses 3
conditions:

- GraphVerified
- CustomResourceDefinitionSynced
- ReconcilerReady

A good start but we violate the principle of a standard top-level condition,
Ready. This should be added.

GraphVerified is hard to link to real code at the moment because the setting of
this value is abstracted.

CustomResourceDefinitionSynced is not a great name because what we really want
to talk about is all of the tasks around creating, updating, and watching the
CRD is accepted. I suspect we don’t watch the CRD very closely as I see cases
where I can delete the CRD KRO has made for me and the controller does not bring
it back.

##### Example

The reconciler would be updated to remove the setConditions methods and replaced
with something more like (these are just off the cuff examples):

```
rdg.Status.PropagateCRDStatus(currentCRD)
rgd.Status.MarkCRDFailed(errorMessage)

rdg.Status.PropagateResourceGraphState(listOfResources)
rgd.Status.MarkFailedResourceUpdate(errorMessage)
```

Status would be updated as follows:

`Current RGD Status`

```
status:
  conditions:
  - lastTransitionTime: "2025-03-24T21:31:02Z"
    message: micro controller is ready
    reason: ""
    status: "True"
    type: ReconcilerReady
  - lastTransitionTime: "2025-03-24T21:31:02Z"
    message: Directed Acyclic Graph is synced
    reason: ""
    status: "True"
    type: GraphVerified
  - lastTransitionTime: "2025-03-24T21:31:02Z"
    message: Custom Resource Definition is synced
    reason: ""
    status: "True"
    type: CustomResourceDefinitionSynced
```

`Proposed Update`

```
status:
  conditions:
  - lastTransitionTime: "2025-03-24T21:31:02Z"
    message: ""
    observedGeneration: 1
    reason: "AllReady"
    status: "True"
    type: Ready
  - lastTransitionTime: "2025-03-24T21:31:02Za"
    message: All resources are up to date and ready.
    observedGeneration: 1
    reason: "AllReady"
    status: "True"
    type: ResourceGraphReady
  - lastTransitionTime: "2025-03-24T21:31:02Z"
    message: MyCustomKind is up to date and accepted.
    observedGeneration: 1
    reason: "Accepted"
    status: "True"
    type: ResourceGraphKindReady
```

Things to note:

- The messages have been customized to have more context for this type. In the
  above example we are working on the RGD for MyCustomKind kind.
- ObservedGeneration is added and used in the conditions.
- The condition Types have been renamed to something relevant to the work that
  is done inside the controller, in this case:
  - GraphVerified → ResourceGraphReady
  - CustomResourceDefinitionSynced → ResourceGraphKindReady
- It is implied that Ready is controlled by a manager and never set directly.

##### Resource Graph Instance

This one is currently pretty tricky because it accepts conditions to be defined
inside the RGD Instance. But at minimum and by default the controller adds:

- InstanceSynced

To be able to use a condition manager, we would need a signal from the provided
status shape to understand if the defined condition is a dependent condition or
just informational.

A similar example applies to Resource Graph Instance as shown under Resource
Graph Definition.

## Tasks

- [x] [ResourceGraphDefinition] Replace ReconcilerReady with Ready, managed by a
      condition manager.
- [x] [ResourceGraphDefinition] Move the conditions logic into Mark* and
      Propagate* methods, isolating API interaction logic from reconciliation
      logic.
  - [x] This breaks apart the static setting of conditions methods found in
        controller_status.go to be focused on single conditions in areas of
        reconciliation code rather than setting the entire condition block all
        together. Further letting the reconciler be testable and independent in
        of itself.
- [ ] [ResourceGraphDefinition] Delete the Condition struct from
      api/v1alpha1/conditions.go. We will use metav1’s Condition from upstream
      and accept the stricter schema validation that comes with it (also what
      the condition manager allows us to centralize).
  - [ ] This adds ObservedGeneration to Conditions explicitly.
- [x] [ResourceGraphDefinition] Watch and react to Resource Graph CRD changes,
      especially the “Accepted” status of a CRD.
- [ ] [Instance] Document the large chunks of work that the reconciler does,
      each of these will get a sub-resource.
- [ ] [Instance] Introduce a signal in the schema of provided conditions to
      determine if a condition should be considered for Readiness or it is just
      informational.
  - [ ] We might also need to support defining the polarity of success, which is
        a really good reason to fork the condition managers, as they don’t
        support this at the moment.
- [ ] [Instance] Introduce the Ready condition as a top-level condition for all
      Instance schemas.

There is likely more work that is not listed above but will require discovery.

## Other solutions considered

- `kstatus`, but rejected based on above opinions.

- No change, but rejected because we do not use observed generation today.

## Discussion and notes

- Scott Nichols Proposed this on March 24, 2024 via
  [Google Doc](https://docs.google.com/document/d/1-Wc8m9LH6wq0URIPqzdpHc_MfQzHfScP5PSzzFCLaxw/edit?tab=t.0)
