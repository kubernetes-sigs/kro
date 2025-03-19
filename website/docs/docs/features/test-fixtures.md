---
sidebar_position: 1
---

# Test Fixtures for Resource Graph Definitions

## Problem Statement

Creating a new ResourceGraphDefinition (RGD) for non-trivial resources requires significant iterative work and validation. The current process involves:

1. Creating an RGD
2. Applying it to the cluster
3. Waiting for status updates
4. Creating an instance
5. Validating the output
6. Deleting and repeating until proper lifecycle support is implemented

This workflow becomes increasingly complex and time-consuming as more conditionals are added, making it difficult to validate that each input produces the expected output.

## Proposed Solution

We propose implementing test fixtures inspired by kubebuilder-declarative-pattern and Kyverno. This solution will allow developers to validate RGDs locally without deploying to a cluster.

### Example Implementation

Given an RGD definition (`rgd.yaml`):

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: example
spec:
  schema:
    apiVersion: v1alpha1
    kind: Example
    spec:
      foo: string | required

  resources:
    - id: cm
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.metadata.name}-cm
        data:
          foo: ${schema.spec.foo}
    - id: cm2
      template:
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: ${schema.metadata.name}-cm2
        data:
          bar: ${schema.spec.foo}
```

Developers can create test files to validate the RGD:

#### Happy Path Test (`tests/happy.yaml`):
```yaml
apiVersion: tests.kro.run/v1alpha1
kind: Expectations
spec: {}
---
apiVersion: kro.run/v1alpha1
kind: Example
metadata:
  name: foobar
  namespace: sparkles
spec:
  foo: hello world!
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: foobar-cm
  namespace: sparkles
data:
  foo: hello world!
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: foobar-cm2
  namespace: sparkles
data:
  bar: hello world!
```

#### Failure Test (`tests/sad.yaml`):
```yaml
apiVersion: tests.kro.run/v1alpha1
kind: Expectations
spec:
  result: Failure
  message:
    contains: foo is a required field
---
apiVersion: kro.run/v1alpha1
kind: Example
metadata:
  name: foobar
  namespace: sparkles
spec: {}
```

### Usage

Tests can be run using a CLI command:

```bash
kro test -f rgd.yaml tests/
```

## Benefits

1. **Faster Development Cycle**: Developers can validate RGDs locally without cluster deployment
2. **Better Testing**: Comprehensive test coverage for both success and failure cases
3. **Improved Validation**: Easy to verify complex conditional logic and template expansions
4. **Documentation as Tests**: Test files serve as documentation for expected behavior

## Implementation Considerations

1. **Test Format**: While YAML is proposed, other formats could be considered:
   - DSL (Domain Specific Language)
   - Native language tests
   - JSON or other structured formats

2. **Validation Features**:
   - Template expansion validation
   - Schema validation
   - Conditional logic testing
   - Resource relationship validation

3. **CLI Integration**:
   - Support for multiple test files
   - Test result reporting
   - Debug mode for detailed output

## Alternatives Considered

1. **DSL Implementation**:
   - Pros: More expressive, programmatic testing
   - Cons: Requires embedding a runtime, adds complexity

2. **Native Language Tests**:
   - Pros: Full programming language capabilities
   - Cons: Less accessible, requires more setup

3. **Cluster-based Testing**:
   - Pros: Real-world validation
   - Cons: Slow, resource-intensive, requires cluster access

## Next Steps

1. Implement basic test fixture support
2. Add CLI command for test execution
3. Create documentation and examples
4. Add support for more complex test scenarios
5. Integrate with CI/CD pipelines

## Community Feedback

We welcome feedback on this proposal, particularly regarding:
- Test file format and structure
- Validation features
- CLI interface design
- Integration with existing tools 