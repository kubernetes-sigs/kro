#!/usr/bin/env bash
# Downloads the upstream crd-ref-docs templates at the pinned version, applies
# kro-specific patches (Docusaurus front matter, kroFieldDoc helper), generates
# the API reference, then discards the temp templates.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

CRD_REF_DOCS_VERSION="${CRD_REF_DOCS_VERSION:-v0.3.0}"
GOMODCACHE="$(go env GOMODCACHE)"
UPSTREAM="${GOMODCACHE}/github.com/elastic/crd-ref-docs@${CRD_REF_DOCS_VERSION}/templates/markdown"
PATCH="${REPO_ROOT}/scripts/hack/crd-ref-docs/templates.patch"
CONFIG="${REPO_ROOT}/scripts/hack/crd-ref-docs/config.yaml"
OUTPUT="${REPO_ROOT}/website/docs/api/api-reference.md"
BIN="${REPO_ROOT}/bin/crd-ref-docs"

if [ ! -d "${UPSTREAM}" ]; then
  echo "ERROR: upstream templates not found at ${UPSTREAM}"
  echo "Run 'make crd-ref-docs' first to populate the Go module cache."
  exit 1
fi

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

cp "${UPSTREAM}"/*.tpl "${TMPDIR}/"
chmod u+w "${TMPDIR}"/*.tpl

patch -d "${TMPDIR}" -p1 --quiet < "${PATCH}"

"${BIN}" \
  --source-path=./api \
  --config="${CONFIG}" \
  --renderer=markdown \
  --templates-dir="${TMPDIR}" \
  --output-path="${OUTPUT}" \
  --log-level=ERROR
