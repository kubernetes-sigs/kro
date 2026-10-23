#!/usr/bin/env bash
# Verifies that the generated API reference is up-to-date.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "${REPO_ROOT}"

echo "Running make generate-api-docs..."
make generate-api-docs

echo "Checking for uncommitted changes..."
if [ -z "$(git status --porcelain -- website/docs/api/api-reference.md)" ]; then
  echo "API reference is up-to-date."
  exit 0
fi

echo "ERROR: API reference is out of date. Run 'make generate-api-docs' and commit the result."
git status --short -- website/docs/api/api-reference.md
git diff -- website/docs/api/api-reference.md
exit 1
