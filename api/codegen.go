// These generate directives are kept separate from the main package to enable
// running them without also running the license headers and attribution checks.
//
// To run everything, from the repo root do go generate ./...
// To run just the CRD related codegen, from the repo root do go generate ./api/...
package api

//go:generate go tool controller-gen object crd paths="./..." output:crd:artifacts:config=../helm/templates/crds
