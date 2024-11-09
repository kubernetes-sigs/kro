#!/usr/bin/env bash
set -euo pipefail

K8S_VERSION="${K8S_VERSION:="1.29.x"}"
KUBEBUILDER_ASSETS="/usr/local/kubebuilder/bin"

main() {
    tools
    kubebuilder
}

tools() {
    if ! echo "$PATH" | grep -q "${GOPATH:-undefined}/bin\|$HOME/go/bin"; then
        echo "Go workspace's \"bin\" directory is not in PATH. Run 'export PATH=\"\$PATH:\${GOPATH:-\$HOME/go}/bin\"'."
    fi

    go install github.com/awslabs/attribution-gen@latest
    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    go install github.com/google/ko@latest
    go install github.com/mikefarah/yq/v4@latest
    go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
    go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
    go install github.com/sigstore/cosign/v2/cmd/cosign@latest
    go install golang.org/x/vuln/cmd/govulncheck@latest
}