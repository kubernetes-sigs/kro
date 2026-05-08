---
sidebar_position: 3
---

# CEL Libraries

kro includes a rich set of CEL function libraries from three sources: kro's own custom libraries, the [cel-go](https://github.com/google/cel-go) standard extensions, and [Kubernetes apiserver](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library) libraries.

## Library Overview

| Library                     | Provider   | Docs |
|-----------------------------|------------|------|
| Hash                        | kro        | [kro](#hash), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#Hash) |
| JSON                        | kro        | [kro](#json), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#JSON) |
| Random                      | kro        | [kro](#random), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#Random) |
| Maps                        | kro        | [kro](#maps), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#Maps) |
| Index Mutation              | kro        | [kro](#index-mutation), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#Lists) |
| Omit                        | kro        | [kro](#omit), [Go doc](https://pkg.go.dev/github.com/kubernetes-sigs/kro/pkg/cel/library#Omit) |
| Policy                      | kro        | [Lifecycle](./02-resource-definitions/06-lifecycle.md) |
| Strings                     | cel-go     | [kro](#strings), [Go doc](https://pkg.go.dev/github.com/google/cel-go/ext#Strings) |
| Lists (cel-go)              | cel-go     | [kro](#lists-cel-go), [Go doc](https://pkg.go.dev/github.com/google/cel-go/ext#Lists) |
| Encoders                    | cel-go     | [kro](#encoders), [Go doc](https://pkg.go.dev/github.com/google/cel-go/ext#Encoders) |
| Two-Variable Comprehensions | cel-go     | [kro](#two-variable-comprehensions), [Go doc](https://pkg.go.dev/github.com/google/cel-go/ext#TwoVarComprehensions) |
| URLs                        | kubernetes | [kro](#urls), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#URLs), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-url-library) |
| Regex                       | kubernetes | [kro](#regex), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Regex), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-regex-library) |
| Quantity                    | kubernetes | [kro](#quantity), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Quantity), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-quantity-library) |
| Lists (k8s)                 | kubernetes | [kro](#lists-k8s), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Lists), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-list-library) |
| IP                          | kubernetes | [kro](#ip), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#IP), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-ip-address-library) |
| CIDR                        | kubernetes | [kro](#cidr), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#CIDR), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-cidr-library) |
| Semver                      | kubernetes | [kro](#semver), [Go doc](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#SemverLib), [k8s.io](https://kubernetes.io/docs/reference/using-api/cel/#kubernetes-semver-library) |


## kro Libraries

### Hash

Cryptographic and non-cryptographic hash functions. All return raw bytes; use `"%x".format(...)` for hex or `base64.encode()` for base64.

| Function | Returns | Description |
|---|---|---|
| `hash.fnv64a(string)` | `bytes` | FNV-1a 64-bit hash. Fast, non-cryptographic. Recommended for most use cases. |
| `hash.sha256(string)` | `bytes` | SHA-256 cryptographic hash. |
| `hash.md5(string)` | `bytes` | MD5 hash. |

**Examples:**

```kro
# SHA-256 as hex string (like sha256sum output)
checksum: ${"%x".format([hash.sha256(schema.spec.config)])}

# FNV-1a as hex (short, fast identifier)
id: ${"%x".format([hash.fnv64a(schema.spec.name + "-" + schema.spec.namespace)])}

# Base64-encoded hash (more compact)
encoded: ${base64.encode(hash.fnv64a(schema.metadata.uid))}
```

### JSON

Parse and serialize JSON strings.

| Function | Returns | Description |
|---|---|---|
| `json.unmarshal(string)` | `dyn` | Parse a JSON string into a CEL value (map, list, string, number, bool, or null). |
| `json.marshal(dyn)` | `string` | Serialize a CEL value to a JSON string. |

**Examples:**

```kro
# Parse a JSON string from a ConfigMap or user input
config: ${json.unmarshal(schema.spec.jsonConfig)}

# Access nested fields after parsing
dbHost: ${json.unmarshal(configmap.data.settings).database.host}

# Serialize structured data into a JSON string (e.g. for an annotation or env var)
configJson: ${json.marshal({"name": schema.spec.name, "replicas": schema.spec.replicas})}
```

### Random

Deterministic random generation seeded by a stable value (e.g. resource UID), so results are reproducible across reconcile loops.

| Function | Returns | Description |
|---|---|---|
| `random.seededString(int, string)` | `string` | Random alphanumeric string of given length, seeded by the second argument. |
| `random.seededInt(int, int, string)` | `int` | Random integer in `[min, max)`, seeded by the third argument. |

**Examples:**

```kro
# Generate a stable random suffix
suffix: ${random.seededString(8, schema.metadata.uid)}

# Generate a stable random port
port: ${random.seededInt(30000, 32768, schema.metadata.uid)}
```

### Maps

Map manipulation functions.

| Function | Returns | Description |
|---|---|---|
| `<map>.merge(map)` | `map` | Merge two maps. Keys from the second map overwrite keys in the first. Keys must be strings. |

**Examples:**

```kro
# Merge user labels with default labels
labels: ${{"app": schema.spec.name, "managed-by": "kro"}.merge(schema.spec.extraLabels)}

# Layer overrides on top of defaults
config: ${{"timeout": "30s", "retries": "3"}.merge(schema.spec.overrides)}
```

### Index Mutation

Pure list functions that return a new list without modifying the input.

| Function | Signature | Description |
|---|---|---|
| `lists.setAtIndex` | `list(T), int, T -> list(T)` | Replace the element at `index`. Index must be in `[0, size(list))`. |
| `lists.insertAtIndex` | `list(T), int, T -> list(T)` | Insert `value` before `index`. Use `index == size(list)` to append. Index must be in `[0, size(list)]`. |
| `lists.removeAtIndex` | `list(T), int -> list(T)` | Remove the element at `index`. Index must be in `[0, size(list))`. |

**Examples:**

```kro
# Replace the second tag
tags: ${lists.setAtIndex(schema.spec.tags, 1, "new-tag")}

# Prepend an environment variable
envVars: ${lists.insertAtIndex(schema.spec.envVars, 0, "DEBUG=true")}

# Remove the first port
ports: ${lists.removeAtIndex(schema.spec.ports, 0)}

# Chain operations: swap first two elements
swapped: ${lists.setAtIndex(lists.setAtIndex(schema.spec.items, 0, schema.spec.items[1]), 1, schema.spec.items[0])}
```

### Omit

The `omit()` sentinel tells kro to remove a field from the rendered resource. This is useful for conditionally excluding fields.

| Function | Returns | Description |
|---|---|---|
| `omit()` | `omit` | Returns a sentinel value that causes the field to be excluded from the output. |

**Examples:**

```kro
# Conditionally include a field
nodeSelector: ${schema.spec.pinToNode ? {"kubernetes.io/hostname": schema.spec.nodeName} : omit()}
```

:::note
`omit()` is only allowed in resource template fields. It is rejected in `includeWhen`, `readyWhen`, and `forEach` expressions.
:::

### Policy

The `policy()` builder defines resource lifecycle behavior. See [Lifecycle](./02-resource-definitions/06-lifecycle.md) for full documentation.

| Function | Returns | Description |
|---|---|---|
| `policy()` | `Policy` | Creates an empty lifecycle policy builder. |
| `<Policy>.withRetain()` | `Policy` | Resource is orphaned (KRO labels removed) on instance deletion. |
| `<Policy>.withDelete()` | `Policy` | Resource is deleted on instance deletion (default). |

**Examples:**

```kro
# Retain storage on deletion
lifecycle: "${policy().withRetain()}"

# Conditional retention
lifecycle: "${schema.spec.env == 'prod' ? policy().withRetain() : policy().withDelete()}"
```


## cel-go Libraries

### Strings

Extended string manipulation functions from [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Strings).

| Function | Returns | Description |
|---|---|---|
| `<string>.charAt(int)` | `string` | Character at position. |
| `<string>.indexOf(string)` | `int` | Index of first occurrence, or `-1`. |
| `<string>.indexOf(string, int)` | `int` | Same, starting search from offset. |
| `<string>.lastIndexOf(string)` | `int` | Index of last occurrence, or `-1`. |
| `<string>.lastIndexOf(string, int)` | `int` | Same, with max search position. |
| `<string>.lowerAscii()` | `string` | Lowercase ASCII characters. |
| `<string>.upperAscii()` | `string` | Uppercase ASCII characters. |
| `<string>.replace(string, string)` | `string` | Replace all occurrences. |
| `<string>.replace(string, string, int)` | `string` | Replace up to N occurrences. |
| `<string>.split(string)` | `list(string)` | Split by separator. |
| `<string>.split(string, int)` | `list(string)` | Split with limit. |
| `<string>.substring(int)` | `string` | Substring from position to end. |
| `<string>.substring(int, int)` | `string` | Substring from start (inclusive) to end (exclusive). |
| `<string>.trim()` | `string` | Remove leading and trailing whitespace. |
| `<string>.reverse()` | `string` | Reverse the string. |
| `<list(string)>.join()` | `string` | Concatenate list elements. |
| `<list(string)>.join(string)` | `string` | Concatenate with separator. |
| `<string>.format(list)` | `string` | Printf-style formatting (`%s`, `%d`, `%f`, `%e`, `%b`, `%x`, `%X`, `%o`). |
| `strings.quote(string)` | `string` | Escape a string for safe printing. |

**Examples:**

```kro
# Printf-style formatting
roleArn: ${"arn:aws:iam::%s:role/%s".format([schema.spec.accountId, schema.spec.roleName])}

# Join list elements with a separator
labelStr: ${schema.spec.tags.join(",")}

# Split a domain name
parts: ${schema.spec.fqdn.split(".")}

# Substring extraction
prefix: ${schema.spec.name.substring(0, 5)}

# Case conversion
lower: ${schema.spec.input.lowerAscii()}
upper: ${schema.spec.input.upperAscii()}

# Search within strings
hasPort: ${schema.spec.endpoint.indexOf(":") >= 0}

# Replace characters
sanitized: ${schema.spec.name.replace("_", "-")}

# Trim whitespace
cleaned: ${schema.spec.input.trim()}

# Hex encoding via %x (useful for hash output)
checksum: ${"%x".format([hash.sha256(schema.spec.data)])}
```

### Lists (cel-go)

Extended list manipulation functions from [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Lists).

| Function | Returns | Description |
|---|---|---|
| `<list>.slice(int, int)` | `list` | Sub-list from start (inclusive) to end (exclusive). |
| `<list>.flatten()` | `list` | Flatten nested lists one level deep. |
| `<list>.flatten(int)` | `list` | Flatten up to N levels. |
| `<list>.sort()` | `list` | Sort comparable elements. |
| `<list>.sortBy(var, keyExpr)` | `list` | Sort by a key expression. |
| `<list>.distinct()` | `list` | Remove duplicate elements. |
| `<list>.reverse()` | `list` | Reverse element order. |
| `lists.range(int)` | `list(int)` | Generate `[0, 1, ..., n-1]`. |

**Examples:**

```kro
# Slice a list
subset: ${schema.spec.items.slice(0, 3)}

# Sort and deduplicate
tags: ${schema.spec.tags.sort().distinct()}

# Sort by a field
ordered: ${items.sortBy(i, i.data.priority)}

# Generate index range
indices: ${lists.range(schema.spec.count)}

# Flatten nested lists
flat: ${schema.spec.nestedPorts.flatten()}
```

### Encoders

Base64 encoding and decoding from [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#Encoders).

| Function | Returns | Description |
|---|---|---|
| `base64.encode(bytes)` | `string` | Encode bytes to base64 string. |
| `base64.decode(string)` | `bytes` | Decode base64 string to bytes. |

**Examples:**

```kro
# Encode a hash to base64
encoded: ${base64.encode(hash.sha256(schema.spec.data))}

# Encode a string as base64 (e.g. for a Kubernetes Secret)
secret: ${base64.encode(bytes(schema.spec.password))}

# Decode a base64-encoded string
decoded: ${string(base64.decode(schema.spec.encodedValue))}
```

### Two-Variable Comprehensions

Two-variable versions of list/map comprehension macros from [cel-go/ext](https://pkg.go.dev/github.com/google/cel-go/ext#TwoVarComprehensions). These provide access to both the key/index and value in each iteration.

| Macro | Description |
|---|---|
| `<list\|map>.all(k, v, predicate)` | True if all entries satisfy the predicate. |
| `<list\|map>.exists(k, v, predicate)` | True if any entry satisfies the predicate. |
| `<list\|map>.existsOne(k, v, predicate)` | True if exactly one entry satisfies the predicate. |
| `<list\|map>.transformList(k, v, transform)` | Convert map/list to a list by evaluating transform for each entry. |
| `<list\|map>.transformList(k, v, filter, transform)` | Same with a filter predicate. |
| `<list\|map>.transformMap(k, v, transform)` | Transform values while preserving keys. |
| `<list\|map>.transformMap(k, v, filter, transform)` | Same with a filter predicate. |
| `<list\|map>.transformMapEntry(k, v, transform)` | Build new key-value pairs (transform must return a single-entry map). |
| `<list\|map>.transformMapEntry(k, v, filter, transform)` | Same with a filter predicate. |

**Examples:**

```kro
# Extract keys from a map
keys: ${{ "app": "nginx", "version": "1.19" }.transformList(k, v, k)}
# -> ["app", "version"]

# Transform map values
scaled: ${{ "cpu": 2, "memory": 4 }.transformMap(k, v, v * 2)}
# -> {"cpu": 4, "memory": 8}

# Filter and transform
filtered: ${{ "a": 1, "b": 5, "c": 3 }.transformMap(k, v, v > 1, v * 10)}
# -> {"b": 50, "c": 30}

# Swap keys and values
swapped: ${{ "us-east-1": "primary", "eu-west-1": "secondary" }.transformMapEntry(k, v, {v: k})}
# -> {"primary": "us-east-1", "secondary": "eu-west-1"}

# Build formatted strings from key-value pairs
envVars: ${{ "APP": "nginx", "PORT": "8080" }.transformList(k, v, k + "=" + v)}
# -> ["APP=nginx", "PORT=8080"]

# Two-variable all/exists on a map
allDifferent: ${{ "hello": "world", "taco": "bell" }.all(k, v, k != v)}
```


## Kubernetes Libraries

### URLs

Parse and inspect URLs. The URL must be an absolute URI or an absolute path.

| Function | Returns | Description |
|---|---|---|
| `url(string)` | `URL` | Parse a string into a URL. Errors if invalid. |
| `isURL(string)` | `bool` | Returns true if the string is a valid URL. |
| `<URL>.getScheme()` | `string` | Returns the URL scheme (e.g. `https`). Empty string if absent. |
| `<URL>.getHost()` | `string` | Returns the host including port (e.g. `example.com:80`). IPv6 addresses are bracketed. |
| `<URL>.getHostname()` | `string` | Returns the hostname without port. IPv6 addresses returned without brackets. |
| `<URL>.getPort()` | `string` | Returns the port, or empty string if not specified. |
| `<URL>.getEscapedPath()` | `string` | Returns the URL-escaped path. |
| `<URL>.getQuery()` | `map(string, list(string))` | Returns query parameters as a map. |

**Examples:**

```kro
# Extract the host from an endpoint URL
host: ${url(schema.spec.endpoint).getHostname()}

# Validate a URL
valid: ${isURL(schema.spec.callbackUrl)}

# Build a connection using URL parts
port: ${url(database.status.endpoint).getPort()}
```

### Regex

Regular expression matching and extraction via [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Regex).

| Function | Returns | Description |
|---|---|---|
| `<string>.find(string)` | `string` | Returns the first regex match, or empty string. |
| `<string>.findAll(string)` | `list(string)` | Returns all non-overlapping matches. |
| `<string>.findAll(string, int)` | `list(string)` | Returns up to N matches. |

:::note
The built-in CEL `<string>.matches(string)` function is always available (it's part of the CEL standard library, not this extension). It returns `true` if the entire string matches the regex.
:::

**Examples:**

```kro
# Validate a resource name pattern
valid: ${schema.spec.name.matches('^[a-z][a-z0-9-]*$')}

# Extract version numbers from a string
versions: ${"v1.2.3 and v4.5.6".findAll('[0-9]+\\.[0-9]+\\.[0-9]+')}

# Find first match
version: ${schema.spec.image.find('[0-9]+\\.[0-9]+\\.[0-9]+')}
```

### Quantity

Work with [Kubernetes resource quantities](https://pkg.go.dev/k8s.io/apimachinery/pkg/api/resource#Quantity) (e.g. `100m`, `1Gi`, `500Mi`).

| Function | Returns | Description |
|---|---|---|
| `quantity(string)` | `Quantity` | Parse a Kubernetes quantity string. |
| `isQuantity(string)` | `bool` | True if the string is a valid quantity. |
| `<Quantity>.add(Quantity\|int)` | `Quantity` | Sum of two quantities. |
| `<Quantity>.sub(Quantity\|int)` | `Quantity` | Difference of two quantities. |
| `<Quantity>.isLessThan(Quantity)` | `bool` | Comparison. |
| `<Quantity>.isGreaterThan(Quantity)` | `bool` | Comparison. |
| `<Quantity>.compareTo(Quantity)` | `int` | Returns `-1`, `0`, or `1`. |
| `<Quantity>.asInteger()` | `int` | Scaled value as integer. Errors on overflow or precision loss. |
| `<Quantity>.asApproximateFloat()` | `float` | Approximate float value. |
| `<Quantity>.isInteger()` | `bool` | True if the quantity can be represented as an integer without loss. |
| `<Quantity>.sign()` | `int` | Returns `1`, `-1`, or `0`. |

**Examples:**

```kro
# Compare memory requests
needsMore: ${quantity(schema.spec.memoryRequest).isLessThan(quantity('1Gi'))}

# Arithmetic on quantities
total: ${quantity(schema.spec.cpuRequest).add(quantity('100m'))}
remaining: ${quantity('2Gi').sub(quantity(schema.spec.memoryRequest))}

# Validate a resource quantity
valid: ${isQuantity(schema.spec.cpuLimit)}

# Convert whole-number quantities to integer
megabytes: ${quantity(schema.spec.storageSize).asInteger()}
```

### Lists (k8s)

Kubernetes-specific list utility functions from [k8s.io/apiserver/pkg/cel/library](https://pkg.go.dev/k8s.io/apiserver/pkg/cel/library#Lists).

| Function | Returns | Description |
|---|---|---|
| `<list>.isSorted()` | `bool` | True if elements are in sorted order. |
| `<list>.sum()` | `T` | Sum of numeric or duration elements. Returns `0` for empty lists. |
| `<list>.min()` | `T` | Minimum element. Errors on empty list. |
| `<list>.max()` | `T` | Maximum element. Errors on empty list. |
| `<list>.indexOf(T)` | `int` | Index of first occurrence, or `-1`. |
| `<list>.lastIndexOf(T)` | `int` | Index of last occurrence, or `-1`. |

**Examples:**

```kro
# Check if ports are sorted
sorted: ${schema.spec.ports.isSorted()}

# Sum all replica counts
totalReplicas: ${schema.spec.replicaCounts.sum()}

# Find min/max
minPort: ${schema.spec.ports.min()}
maxPort: ${schema.spec.ports.max()}

# Find element index
idx: ${schema.spec.tags.indexOf("production")}
```

### IP

Parse and inspect IPv4 and IPv6 addresses. IPv4-mapped IPv6 addresses and addresses with zones are not allowed.

| Function | Returns | Description |
|---|---|---|
| `ip(string)` | `IP` | Parse a string into an IP address. Errors if invalid. |
| `isIP(string)` | `bool` | Returns true if the string is a valid IPv4 or IPv6 address. |
| `ip.isCanonical(string)` | `bool` | Returns true if the string is in canonical IP form. |
| `<IP>.family()` | `int` | Returns `4` or `6`. |
| `<IP>.isLoopback()` | `bool` | True for `127.x.x.x` (v4) or `::1` (v6). |
| `<IP>.isGlobalUnicast()` | `bool` | True for globally routable addresses. |
| `<IP>.isLinkLocalUnicast()` | `bool` | True for link-local unicast (`169.254.x.x` / `fe80::/10`). |
| `<IP>.isLinkLocalMulticast()` | `bool` | True for link-local multicast (`224.0.0.x` / `ff00::/8`). |
| `<IP>.isUnspecified()` | `bool` | True for `0.0.0.0` or `::`. |
| `string(<IP>)` | `string` | Convert back to string. |

**Examples:**

```kro
# Validate an IP address from user input
ready: ${isIP(schema.spec.clusterIP)}

# Check IP family for dual-stack logic
family: ${string(ip(schema.spec.podIP).family())}

# Gate on global unicast
isRoutable: ${ip(schema.spec.address).isGlobalUnicast()}

# Canonical form check
canonical: ${ip.isCanonical(schema.spec.ipv6Addr)}
```

### CIDR

Parse and inspect CIDR subnet notation. Works with both IPv4 and IPv6.

| Function | Returns | Description |
|---|---|---|
| `cidr(string)` | `CIDR` | Parse a CIDR string (e.g. `192.168.0.0/24`). Errors if invalid. |
| `isCIDR(string)` | `bool` | Returns true if the string is valid CIDR notation. |
| `<CIDR>.containsIP(string\|IP)` | `bool` | True if the CIDR contains the given IP. |
| `<CIDR>.containsCIDR(string\|CIDR)` | `bool` | True if the CIDR fully contains the given subnet. |
| `<CIDR>.ip()` | `IP` | Returns the address part of the CIDR. |
| `<CIDR>.prefixLength()` | `int` | Returns the prefix length in bits. |
| `<CIDR>.masked()` | `CIDR` | Returns the CIDR in canonical masked form. |
| `string(<CIDR>)` | `string` | Convert back to string. |

**Examples:**

```kro
# Check if a pod IP falls within the service CIDR
inRange: ${cidr(schema.spec.serviceCIDR).containsIP(pod.status.podIP)}

# Validate that a user-provided CIDR is valid
valid: ${isCIDR(schema.spec.subnetCIDR)}

# Extract prefix length
prefixLen: ${string(cidr(schema.spec.subnetCIDR).prefixLength())}

# Check subnet containment
isSubnet: ${cidr('10.0.0.0/8').containsCIDR(schema.spec.vpcCIDR)}
```

### Semver

Parse and compare [semantic versions](https://semver.org). Supports optional normalization to handle common non-strict formats like `v1.0` or `01.02.03`.

| Function | Returns | Description |
|---|---|---|
| `semver(string)` | `Semver` | Parse a strict semver string (e.g. `1.2.3`). |
| `semver(string, bool)` | `Semver` | Parse with normalization (strips `v` prefix, fills missing parts, removes leading zeros). |
| `isSemver(string)` | `bool` | Returns true if the string is a valid semver. |
| `isSemver(string, bool)` | `bool` | Same with optional normalization. |
| `<Semver>.major()` | `int` | Major version number. |
| `<Semver>.minor()` | `int` | Minor version number. |
| `<Semver>.patch()` | `int` | Patch version number. |
| `<Semver>.isGreaterThan(Semver)` | `bool` | True if receiver > operand. |
| `<Semver>.isLessThan(Semver)` | `bool` | True if receiver < operand. |
| `<Semver>.compareTo(Semver)` | `int` | Returns `-1`, `0`, or `1`. |

**Examples:**

```kro
# Validate a version string
validVersion: ${isSemver(schema.spec.version)}

# Compare versions
ready: ${semver(schema.spec.version).compareTo(semver('2.0.0')) >= 0}

# Extract major version for image tag
majorTag: ${string(semver(schema.spec.appVersion).major())}

# Tolerant parsing with normalization (accepts "v1.0", "01.02.03", etc.)
valid: ${isSemver(schema.spec.version, true)}
parsed: ${semver(schema.spec.version, true).minor()}
```

For the complete CEL language reference, see the [CEL language definitions](https://github.com/google/cel-spec/blob/master/doc/langdef.md#list-of-standard-definitions).
