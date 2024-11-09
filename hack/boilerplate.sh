#!/bin/bash
set -e

for i in $(find ./cmd ./internal ./test ./hack -name "*.go"); do
    if ! grep -q "Apache License" "$i"; then
        printf '%s\n%s\n' "$(cat hack/boilerplate.go.txt)" "$(cat $i)" > temp && mv temp "$i"
    fi
done