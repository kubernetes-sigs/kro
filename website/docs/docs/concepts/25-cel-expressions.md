---
sidebar_position: 25
---

# CEL Expressions in Kro

Common Expression Language (CEL) is a powerful expression language used in Kro for resource validation, transformation, and policy enforcement. This guide covers everything you need to know about using CEL in Kro.

## Overview

CEL provides a simple, powerful way to express business logic and validation rules in Kro. It's used for:
- Validating resource configurations
- Transforming data between resources
- Enforcing policies and constraints
- Defining conditional logic

## Available Functions

### Standard Library Functions

#### String Operations
- `size(str)` - Returns the length of a string
- `matches(str, regex)` - Matches a string against a regular expression

#### List Operations
- `size(list)` - Returns the length of a list
- `in(elem, list)` - Checks if an element is in a list
- `filter(list, predicate)` - Filters list elements based on a predicate
- `map(list, transform)` - Transforms each element in a list
- `all(list, condition)` - Checks if all elements satisfy a condition
- `exists(list, condition)` - Checks if any element satisfies a condition
- `exists_one(list, condition)` - Checks if exactly one element satisfies a condition

#### Type Conversions
- `int(value)` - Converts to integer
- `uint(value)` - Converts to unsigned integer
- `double(value)` - Converts to double
- `bool(value)` - Converts to boolean
- `string(value)` - Converts to string
- `bytes(value)` - Converts to bytes
- `timestamp(value)` - Converts to timestamp
- `duration(value)` - Converts to duration
- `type(value)` - Returns the type of a value

## Resource Access

### Basic Resource Access
```cel
// Simple bucket field access
bucket.spec.name == "my-bucket"
bucket.metadata.name == bucket.spec.name

// EKS cluster and deployment access
deployment.metadata.namespace == "default"
eksCluster.spec.version == "1.31"
```

### Working with Lists
```cel
// Filter node groups by state
nodeGroups.filter(ng, ng.status.state == "ACTIVE")

// Check IAM role policies
contains(map(iamRole.policies, p, p.actions), "eks:*")

// Filter VPC subnets
size(vpc.subnets.filter(s, s.isPrivate)) >= 2
```

## Common Use Cases

### Validation Examples
```cel
// EKS cluster state validation
eksCluster.status.state == "ACTIVE" && 
duration(timeAgo(eksCluster.status.createdAt)) > duration("24h") && 
size(nodeGroups.filter(ng,
    ng.status.state == "ACTIVE" &&
    contains(ng.labels, "environment"))) >= 1

// Security group validation
securityGroup.spec.vpcID == vpc.status.vpcID && 
securityGroup.spec.rules.all(r, 
    contains(map(r.ipRanges, range, concat(range.cidr, "/", range.description)), 
        "0.0.0.0/0"))

// Order and inventory validation
order.total > 0 && 
order.items.all(item,
    product.id == item.productId && 
    inventory.stock[item.productId] >= item.quantity)
```

### Transformation Examples
```cel
// Name transformation with functions
concat(toLower(eksCluster.spec.name), "-", bucket.spec.name)

// Complex data transformation
process(
    b"bytes123",         // BytesValue
    3.14,               // DoubleValue
    42u,                // Uint64Value
    null                // NullValue
)

// Struct creation
createPod(Pod{
    metadata: {
        name: "test", 
        labels: {"app": "web"}
    }, 
    spec: {
        containers: [{
            name: "main", 
            image: "nginx"
        }]
    }
})
```

## Debugging and Troubleshooting

### Common Errors

1. Type Mismatch
```cel
// Wrong: Comparing with wrong type
deployment.spec.replicas == "5"

// Correct: Using proper type
deployment.spec.replicas == 5
```

2. Resource Access
```cel
// Wrong: Accessing undefined field
eksCluster.undefined.field

// Correct: Using proper field path
eksCluster.status.state == "ACTIVE"
```

3. Function Usage
```cel
// Wrong: Invalid function arguments
count(deployment)

// Correct: Using proper function
size(deployment.spec.template.spec.containers) > 0
```

### Best Practices

1. Always validate resource state:
```cel
eksCluster.status.state == "ACTIVE"
```

2. Use proper type comparisons:
```cel
max(deployment.spec.replicas, 5)
```

3. Handle collections properly:
```cel
nodeGroups.filter(ng, ng.status.state == "ACTIVE")
```

## Security Considerations

1. Avoid sensitive data in expressions
2. Use appropriate access controls
3. Validate user input before using in expressions
4. Set reasonable timeouts for expression evaluation
5. Consider resource implications of complex expressions

## Performance Tips

1. Use early exits for complex conditions
2. Avoid unnecessary type conversions
3. Cache frequently used values in variables
4. Use appropriate data structures
5. Keep expressions simple and focused

## Additional Resources

- [CEL Language Specification](https://github.com/google/cel-spec)
- [CEL Go Implementation](https://github.com/google/cel-go)
- [Kubernetes CEL Documentation](https://kubernetes.io/docs/reference/using-api/cel/) 