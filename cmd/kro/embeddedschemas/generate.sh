#!/usr/bin/env bash
# Copyright 2025 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

SCHEMAS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

MIN_VERSION=30
MAX_VERSION=36

echo "Generating Kubernetes OpenAPI schemas from kubernetes/kubernetes"
echo ""

mkdir -p "${SCHEMAS_DIR}"

for minor in $(seq ${MIN_VERSION} ${MAX_VERSION}); do
  version="v1.${minor}"
  git_tag="${version}.0"
  url="https://raw.githubusercontent.com/kubernetes/kubernetes/${git_tag}/api/openapi-spec/swagger.json"
  schemas_file="${SCHEMAS_DIR}/swagger_${version}.json"

  echo "Downloading ${version} from ${url}"

  curl -fsSL "${url}" -o "${schemas_file}"
  if [ ! -s "${schemas_file}" ]; then
    echo "ERROR: Failed to download ${version}" >&2
    exit 1
  fi

  echo "Wrote ${schemas_file}"
done

echo ""
echo "All schemas generated successfully"
echo "Definitions and scopes are extracted at runtime"
echo ""
echo "To commit: git add ${SCHEMAS_DIR}/*.json"
