#!/bin/bash

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KRO_RENDER="${KRO_RENDER:-$REPO_ROOT/bin/kro-render}"

if [[ ! -x "$KRO_RENDER" ]]; then
  echo "kro-render not found. Build it with: make kro-render" >&2
  exit 1
fi

exec "$KRO_RENDER" "$@"
