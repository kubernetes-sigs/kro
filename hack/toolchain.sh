#!/usr/bin/env bash
set -euo pipefail

main() {
    tools
    kubebuilder
}

tools() {
    if ! echo "$PATH" | grep -q "${GOPATH:-undefined}/bin\|$HOME/go/bin"; then
        echo "Go workspace's \"bin\" directory is not in PATH. Run 'export PATH=\"\$PATH:\${GOPATH:-\$HOME/go}/bin\"'."
    fi

    go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
    go install github.com/google/ko@latest
    go install github.com/mikefarah/yq/v4@latest
    go install github.com/sigstore/cosign/v2/cmd/cosign@latest
    go install golang.org/x/vuln/cmd/govulncheck@latest
}
