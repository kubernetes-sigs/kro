---
sidebar_position: 3
---

# Modifiers

`includeWhen`, `readyWhen`, and `forEach` are the same fields, with the same
meaning, in a Graph node and an RGD resource. Their full semantics are
documented once, under [Reconciliation](../reconciliation/01-conditional-creation.md).
This page covers only what is specific to Graphs.

## Support by Node Kind

| | `template` | `ref` | `def` | `patch` | `graph` |
| --- | --- | --- | --- | --- | --- |
| `includeWhen` | Yes | Yes | Yes | Yes | No |
| `readyWhen` | Yes | Yes | Yes | No | No |
| `forEach` | Yes | No | Yes | Yes | No |

A Graph that uses a modifier on a node kind that does not support it is rejected
when it is compiled, with `Accepted=False`.

- **`graph` nodes** accept no modifiers. Apply them to the nodes inside the
  nested Graph instead.
- **`ref` nodes** reject `forEach`. To read many resources, use a `selector`
  in the `ref`; the node's value is then a list.
- **`patch` nodes** accept `forEach`, which fans the same contribution out to
  one target per iteration. Every iterator must appear in `metadata.name` or
  `metadata.namespace` so each target is distinct.
- **`patch` nodes** reject `readyWhen`. A patch contributes fields to its target
  and publishes no value into scope, so there is nothing for `readyWhen` to
  evaluate. To wait on the target's state, add a `ref` node for the target and
  put `readyWhen` on that.

## No `schema` Variable

An RGD's modifiers usually reference the instance spec (`${schema.spec.enabled}`).
A Graph has no instance and no `schema` variable. Conditions and iterators
reference other nodes: a `ref` that reads cluster state, a `def` that computes a
value, or a `template` that has already been applied.

```kro
nodes:
  - id: namespaces
    ref:
      apiVersion: v1
      kind: Namespace
      metadata:
        selector:
          matchLabels:
            policy: enforced

  - id: policies
    forEach:
      - ns: ${namespaces}
    includeWhen:
      - ${size(namespaces) > 0}
    readyWhen:
      - ${each.metadata.name != ""}
    template:
      apiVersion: networking.k8s.io/v1
      kind: NetworkPolicy
      metadata:
        name: default-deny
        namespace: ${ns.metadata.name}
      spec:
        podSelector: {}
        policyTypes: [Ingress, Egress]
```

## `readyWhen` Does Not Gate Dependents

In a Graph, `readyWhen` reports a node's health into the Graph's
`ResourcesConverged` and `Ready` conditions. It does not delay nodes that depend
on it; a dependent is applied as soon as the fields it references exist. This is
the one behavioral difference from an RGD, and it is explained in
[Readiness](../reconciliation/02-readiness.md#dependencies-and-readiness).

## Next Steps

- **[Conditional Creation](../reconciliation/01-conditional-creation.md)** - `includeWhen` semantics
- **[Readiness](../reconciliation/02-readiness.md)** - `readyWhen` semantics and the RGD/Graph difference
- **[Collections](../reconciliation/03-collections.md)** - `forEach` semantics, identity rules, and limits
- **[Nodes](./02-nodes.md)** - The node kinds these modifiers attach to
