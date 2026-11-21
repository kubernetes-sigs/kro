# def for RGD

## Problem statement

RGDs in Kro have no way to store intermediate results of CEL expressions. This
can lead to RGDs with excessive duplication that are hard to maintain.

Some features, such as variables and resource contribution, were indirectly
blocked on the design of Graph. Graph recently launched in alpha and introduced
`def` nodes to solve the variable problem.

Some of these features, like resource contribution, can be solved by creating an
RGD that stamps a Graph resource. Variables are unique in that they can't be
solved this way. A Graph inside an RGD could have only `def` nodes, but it
becomes just data storage, functionally equivalent to a ConfigMap.

## Proposal

Support `def` nodes in RGD.

This RGD computes a naming prefix once and uses it in 2 resources.

```
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: my-app
spec:
  schema:
    apiVersion: v1alpha1
    kind: MyApp
    spec:
      name: string
      environment: string
  resources:
    - id: naming
      def:
        prefix: ${schema.spec.name + '-' + schema.spec.environment}
    - id: config
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${naming.prefix + '-config'}
        data:
          env: ${schema.spec.environment}
    - id: pod
      template:
        apiVersion: v1
        kind: Pod
        metadata:
          name: ${naming.prefix + '-pod'}
          labels:
            app: ${naming.prefix}
        spec:
          containers:
            - name: app
              image: nginx
```

### Implementation

We will need to edit the RGD CRD to add a new untyped `def` field on the same
level as `template` and `externalRef`.

The shared graph engine makes implementing `def` nodes simple, and much of the
work can be reused. The RGD adapter translates a `def` resource into a Graph
`def` node, so the executor needs no change.

#### RGD has its own compile-time type checking

An RGD is compiled by two separate code paths.

- At RGD build time, the RGD controller calls the builder in `pkg/graph`. The
  builder type checks every CEL expression against a typed environment built
  from the schemas of the referenced resources, and it generates the instance
  CRD, including typed status fields. This is where RGD authors get
  compile-time errors today.
- At instance time, the adapter translates the RGD into a Graph, and the Graph
  compiler in `pkg/graphengine/compiler` compiles it.

#### Adding type checking

The RGD builder (`compileSource` in `pkg/graph`) compiles an RGD in these
phases:

1. Validate the RGD. Convert its SimpleSchema into the instance CRD and the
   schema bound to the `schema` CEL variable.
2. Build each resource into a node. Templates and external refs resolve their
   OpenAPI schema. A `def` is parsed without a schema, which finds its CEL
   expressions, and has no schema yet.
3. Build the dependency graph. A names-only inspector finds each node's
   references in its fields, `forEach`, `includeWhen` and `readyWhen`. This
   needs no types. Then check for cycles and topologically sort.
4. **New:** infer `def` schemas. Visit `def` nodes in topological order. For
   each one, assemble the schemas known so far (resources, earlier `def` nodes
   and `schema`) into a typed CEL environment and compile each of its
   expressions. Build the `def` schema from its literal structure and the
   checked output type of each expression. A `def` with `forEach` types its
   iterator variables from the `forEach` expression, and its schema is wrapped
   as a list. Add the schema to the type map.
5. Build the final typed CEL environment from all schemas and validate every
   node. This checks each expression against the type of its target field, that
   `includeWhen` and `readyWhen` return bool, and that `forEach` returns a
   list. A `def` has no target field, so its expressions only need to compile.
6. Infer the status schema. Status expressions compile against the same
   environment, and their output types become the status schema of the instance
   CRD. A status field that references a `def` field gets that field's type.
7. Emit the Graph and the CRD.

### Graph `def` node compile gaps

In Graph, a `def` node has no target schema, so its schema is inferred from its
value. A literal gets the type of its value. Graph doesn't type the fields of a
`def` that hold CEL expressions, so those fields get the type `dyn`.

For example, the following Graph compiles because `a.v` is `dyn`, even though
the expression `'5'` is a string.

```
nodes:
- id: a
  def:
    v: ${'5'}
- id: b
  def:
    asString: ${a.v + 'x'}
    asInt: ${a.v + 1}
```

It fails at runtime:

```
node "b": eval "a.v + 1" at "asInt": eval "a.v + 1": no such overload
```

This logic cannot simply be reused for RGD type checking because it is not
powerful enough.

After implementing type checking for RGD `def`, we can move the inference into
a shared package that both the Graph compiler and the RGD builder call. Graph
then reports this error at compile time instead of at runtime. This is a
behavior change for Graph: a Graph with a type error in a `def` that compiled
before will fail to compile.

### Feature gating plan

The RGD change will be behind a feature gate for at least a release, in line
with project norms. This allows more stable development and lets us improve the
feature without breaking as many customers.

### Testing

We will add unit, integration, and end-to-end tests that cover the new type
checking and `def` nodes in RGD, plus the parity test described above.

### Alternatives

#### Keep `def` Graph-only

An alternative is to leave `def` as a Graph-only concept. The value `def` adds
to RGD is similar to the value it adds to Graph, so keeping it Graph-only
doesn't make sense.

#### Avoid making updates to Graph

This KREP suggests moving `def` type inference into a shared package, so the
Graph compiler and the RGD builder infer the same types. An alternative is to
add the inference only to the RGD builder and leave Graph as it is.

We prefer the shared package for two reasons. The compiler's current inference
is replaced, not added to, so sharing doesn't add much code. Graph users would 
also not benefit and see runtime errors that we could have found at compile time.

### Differences from KREP

KREP-011 ([#1035](https://github.com/kubernetes-sigs/kro/pull/1035)) proposed
the same feature as a `variable` node. This proposal differs in 4 ways.

- The field is named `def`, not `variable`, to match Graph.
- A `def` runs on the shared graph engine through the RGD adapter. KREP-011
  added a new node type to the RGD runtime.
- A `def` key may match its own id. KREP-011 rejected that. Graph allows it, so
  we follow Graph.
- Type inference works as in KREP-011, but the inference function is shared with
  the Graph compiler, so Graph gets it too.
