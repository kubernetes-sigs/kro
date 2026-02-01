# kro-render

Offline dry-run renderer for RGD templates with instance data. Implements [issue #993](https://github.com/kubernetes-sigs/kro/issues/993).

Renders the fully hydrated RGD template with a provided instance to stdout, without applying to a cluster.

## Build

```bash
make kro-render
# or
go build -o bin/kro-render ./cmd/kro-render
```

## Usage

```bash
# Render RGD with instance file
kro-render -f empty.rgd.yaml -i empty.yaml

# Render using dummy instance from kro generate instance (pipe for dry-run)
kro generate instance -f empty.rgd.yaml | kro-render -f empty.rgd.yaml -i -

# JSON output
kro-render -f empty.rgd.yaml -i empty.yaml -o json
```

## Options

| Flag | Description |
|------|-------------|
| `-f`, `--file` | Path to ResourceGraphDefinition file (required) |
| `-i`, `--instance` | Path to instance file, or `-` for stdin (required) |
| `-o`, `--output` | Output format: `yaml` (default) or `json` |

## Requirements

- **Kubeconfig**: Graph building requires OpenAPI schema resolution. Ensure `KUBECONFIG` or `~/.kube/config` points to a reachable cluster (even if you don't apply resources).
- **Limitation**: RGDs with `readyWhen` that depend on other resources' status cannot be fully rendered offline (no cluster state). Use RGDs where resources only depend on `schema.*` for offline dry-run.

## Example

**empty.yaml** (instance):

```yaml
apiVersion: kro.run/v1alpha1
kind: Empty
metadata:
  name: myempty
spec:
  key: foobar
  value: test
```

**empty.rgd.yaml** (RGD):

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: empty
  labels:
    "kro.run/managed-by": "kro"
spec:
  schema:
    apiVersion: v1alpha1
    kind: Empty
    spec:
      key: string | default="key"
      value: string | default="value"
  resources:
    - id: configmap
      template:
        apiVersion: "v1"
        kind: "ConfigMap"
        metadata:
          name: "empty-configmap"
          labels:
            "kro.run/managed-by": "empty"
            "test": '1'
        data:
          ${schema.spec.key}: "${schema.spec.value}"
```

**Output**:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: empty-configmap
  labels:
    "kro.run/managed-by": "empty"
    "test": "1"
data:
  foobar: test
```
