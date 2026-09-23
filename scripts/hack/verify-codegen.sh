#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

echo "Running go mod tidy..."
go mod tidy

echo "Running make generate..."
make generate

echo "Running make manifests..."
make manifests

# --porcelain rather than `git diff`, so a newly generated file that is not yet
# tracked fails the check instead of passing silently.
if [ -n "$(git status --porcelain)" ]; then
    echo ""
    echo "ERROR: Generated files are out of date. Please run 'make verify-codegen' and commit the result."
    echo ""
    git status --short
    git diff
    exit 1
fi

echo "Generated files are up to date."
