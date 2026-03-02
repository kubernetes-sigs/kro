

# Proposal title

Extend includeWhen to participate in dependency graph construction

## Problem statement
Current Behaviour supports only reference the "schema" variable.
I would like to be able to use externalRef in includeWhen statement. Here is example how it can look like: 
```
`apiVersion: kro.run/v1alpha1
 kind: ResourceGraphDefinition
 metadata:
 name: foo
 spec:
    schema:
    apiVersion: v1
    kind: Foo
    spec:
      crdName: string | required=true description="Name of the CRD"

resources:
- id: serviceAccount
  externalRef:
  apiVersion: v1
  kind: ServiceAccount
  metadata:
     name: service-account-name 
     namespace: ${schema.spec.namespace}

- id: storageBucket
  template:
    apiVersion: storage.cnrm.cloud.google.com/v1beta1
    kind: StorageBucket
    metadata:
      name: ${schema.metadata.name}-bucket
      namespace: ${schema.metadata.namespace}
  includeWhen:
    - ${serviceAccount.metadata.exists(x, x.name == 'test')}`
```

## Proposal

I am proposing to treat includeWhen as CEL expressions the same way `forEach` `readyWhen` are treated . As input to dependency-graph constructions , not just a runtime conditions.

#### Overview

At this point -
includeWhen is a runtime-evaluated only 
it is restricted to scyhema references 
doesnt perticipate in dependecies infers 

so because of these the graph builder ignore resource refrences inside includeWhen. 

#### Design details

1. Allow resource references in includeWhen
2. Use includeWhen expressions to infer dependencies (graphtime)
3. Preserve existing runtime semantics (no behviour change)
4. Add integration tests for chained conditional dependencies  
Introduce integration tests covering scenarios such as:
`A → B → C → D`  (can be added in seprate PR )


## Other solutions considered

inside the issue the solution was referneced to use externalRef resource in includeWhen cel expression to reference to externalRef resource. 
It is not ideal because it treat includeWhen as runtime hack i can say not a grpah signal . it also lead to alot of unsafe evaluation order . 




## Testing strategy

#### Requirements

:memo: What is needed to test this proposed set of changes?
 we will be testing t o verify that dependecies are correctly added at graph-build time.
 it doesnt return to former existing includeWhen behaviour . 


#### Test plan

for graph builder -
Verifing that includeWhen expressions are parsed and translated into dependency edges.
then a integration test -
it should verify ordering and ignore chain propogation at the runtime .
then a regression test - ensuring we dont break existing users . 

### major test to be included
chained includeWhen - 
 testing cases like 
 1. All included 
 2. A ignored 
 3. b ignored 
 this is example scenario for `A → B → C → D`




## Discussion and notes

:memo: Placeholder for taking notes through discussions, if useful.
