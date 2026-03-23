#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

echo "Running make generate..."
make generate

echo "Running make manifests..."
make manifests

if ! git diff --quiet --exit-code; then
    echo ""
    echo "ERROR: Code generation is out of date. Please run 'make generate && make manifests' and commit the result."
    echo ""
    git diff --stat
    exit 1
fi

echo "Code generation is up to date."
