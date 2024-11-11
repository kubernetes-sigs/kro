---
sidebar_position: 2
---

# Simple Schema

kro's Simple Schema provides a powerful yet intuitive way to define the
structure of your ResourceGroup. Here is comprehensive example:

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGroup
metadata:
  name: web-application
spec:
  apiVersion: v1alpha1
  kind: WebApplication
  parameters:
    spec:
      name: string | required=true description="Name of the web application"
      replicas: integer | default=1 minimum=1 maximum=10
      image: string | required=true
      ports:
        - port: integer | required=true
          targetPort: integer
      env: 'map[string]string'
      config: ConfigType
      configArray: []ConfigType
    customTypes:
      ConfigType:
        logLevel: string | enum="debug,info,warn,error" default="info"
        maxConnections: integer | minimum=1 maximum=1000
    status:
      url: ${service.status.loadBalancer.ingress[0].hostname}"
  resources: []
```

## Simple Schema Features Explained

### 1. Spec Field Definition

#### Basic Types

- `string`: Basic string type
- `integer`: Whole number
- `boolean`: True/False value

for example to define a field that is a string, you can define it as follows:

```yaml
name: string
age: integer
```

#### Structure types

Structure types or object types are defined by specifying the fields within the
object. The fields can be of basic types or other structure types.

for example to define a structure type for a person with name and age fields,
you can define it as follows:

```yaml
person:
  name: string
  age: integer
```

#### Map Types

- Arrays: Denoted by `[]`, e.g., `'[]string'`
- Maps: Denoted by `map[keyType]valueType`, e.g., `'map[string]string'` and
  `'map[string]Person'`

### 2. Validation and Documentation Markers

In addition to the type, fields can also have markers for validation,
documentation and other purposes that are OpenAPISchema compatible.

For example to define a field that is required, has a default value and a
description, you can define it as follows:

```yaml
person:
  name:
    string | required=true default="Kylian Mbappé" description="Name of the
    person"
```

Currently supported markers include:

- `required=true`: Field must be provided
- `default=value`: Default value if not provided
- `description="..."`: Provides documentation for the field
- `enum="value1,value2,..."`: Restricts to a set of values **NOT IMPLEMENTED**
- `minimum=value` and `maximum=value`: For numeric constraints **NOT
  IMPLEMENTED**

### 3. Custom Types Definition

Custom types are defined in the `customTypes` section, allowing for reusable
complex structures. They can be referenced by name in the spec or status fields.

Example:

```yaml
customTypes:
  ConfigType:
    logLevel: string | enum="debug,info,warn,error" default="info"
    maxConnections: integer | minimum=1 maximum=1000
spec:
  config: ConfigType | required=true
```

### 4. Status Field Definition

Status fields are defined similarly to spec fields and can include validation
and documentation markers. However on top of that, status fields can also
include value markers:

#### Value Marker **NOT IMPLEMENTED**

- `value="${resource.status.field}"`: Specifies that this field's value should
  be dynamically obtained from another resource. The value is a CEL expression
  that is validated at ResourceGroup processing time and evaluated at runtime.

:::tip Note that the value marker is a kro extension to the OpenAPISchema and is
not part of the official OpenAPISchema specification. :::

Example:

```yaml
status:
  url: string | value="${service.status.loadBalancer.ingress[0].hostname}"
```

## Default status fields

**kro** automatically injects two common fields into the status of all instances
generated from **ResourceGroups**: `conditions` and `state`. These fields
provide essential information about the current status of the instance and its
associated resources.

:::tip `conditions` and `state` are reserved words in the status section. If a
user defines these fields in their **ResourceGroup**'s status schema, kro will
override them with its own values. :::

### 1. Conditions

The `conditions` field is an array of condition objects, each representing a
specific aspect of the instance's state. kro automatically manages this field.

```yaml
status:
  conditions: "[]condition"
customTypes:
  condition:
    type: string
    status: string | enum="True,False,Unknown"
    lastTransitionTime: string
    reason: string
    message: string
```

Common condition types include:

- `Ready`: Indicates whether the instance is fully reconciled and operational.
- `Progressing`: Shows if the instance is in the process of reaching the desired
  state.
- `Degraded`: Signals that the instance is operational but not functioning
  optimally.
- `Error`: Indicates that an error has occurred during reconciliation.

### 2. State

The `state` field provides a high-level summary of the instance's current
status.

```yaml
status:
  state: string | enum="Ready,Progressing,Degraded,Error,Terminating,Unknown"
```

> These default status fields are automatically added to every instance's
> status, providing a consistent way to check the health and state of resources
> across different **ResourceGroups**.
