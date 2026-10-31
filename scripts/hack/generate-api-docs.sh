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
BIN="${CRD_REF_DOCS:-${REPO_ROOT}/bin/crd-ref-docs}"

K8S_API_VERSION="$(go list -m -f '{{.Version}}' k8s.io/api)"
EXPECTED_K8S_VERSION="1.$(echo "${K8S_API_VERSION}" | cut -d. -f2)"
CONFIGURED_K8S_VERSION="$(sed -n 's/^[[:space:]]*kubernetesVersion:[[:space:]]*\"\{0,1\}\([0-9.]*\)\"\{0,1\}.*/\1/p' "${CONFIG}")"
if [ "${CONFIGURED_K8S_VERSION}" != "${EXPECTED_K8S_VERSION}" ]; then
  echo "ERROR: kubernetesVersion in ${CONFIG} is \"${CONFIGURED_K8S_VERSION}\" but go.mod pins k8s.io/api ${K8S_API_VERSION}; set it to \"${EXPECTED_K8S_VERSION}\"." >&2
  exit 1
fi

if [ ! -d "${UPSTREAM}" ]; then
  echo "ERROR: upstream templates not found at ${UPSTREAM}; run 'rm -f ${BIN} && make crd-ref-docs' to repopulate the Go module cache." >&2
  exit 1
fi

if [ ! -x "${BIN}" ]; then
  echo "ERROR: crd-ref-docs not found or not executable at ${BIN}; run 'rm -f ${BIN} && make crd-ref-docs'." >&2
  exit 1
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

cp "${UPSTREAM}"/*.tpl "${WORK_DIR}/"
chmod u+w "${WORK_DIR}"/*.tpl

patch -d "${WORK_DIR}" -p1 --quiet < "${PATCH}"

"${BIN}" \
  --source-path=./api \
  --config="${CONFIG}" \
  --renderer=markdown \
  --templates-dir="${WORK_DIR}" \
  --output-path="${OUTPUT}" \
  --log-level=ERROR
