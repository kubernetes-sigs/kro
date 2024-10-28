# Setup Guide for Envtest

## Install setup-envtest tool

First install the `setup-envtest` tool:

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
```

## Download Kubebuilder assets

Use `setup-envtest` to download the Kubebuilder assets:

```sh
setup-envtest use <version> -p path
```

Replace `<version>` with the Kubernetes version you want to test against, for
example, `1.31.x`. The `-p path` flag will print the path where the assets are stored.

## Set environment variable

Set the `KUBEBUILDER_ASSETS` environment variable to the path printed by the previous
command:

```sh
export KUBEBUILDER_ASSETS=/path/to/kubebuilder/assets
```

You may want to add this to your shells configuration file (e.g `~/.bashrc` or `~/.zshrc`)
to make it permanent.

## Run the tests

```sh
go test ./test/integration/... -v
```

If you're still encountering issues, make sure the `KUBEBUILDER_ASSETS` environment variable
is correctly set and points to a directory containing the `etcd` binary.