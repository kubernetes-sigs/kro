// Package stdlib embeds the standard library resources and provides
// Apply() to install them into a cluster. Used by integration tests
// and the Helm chart bootstrap sequence.
package stdlib

import "embed"

// Resources contains the stdlib YAML files.
//
//go:embed *.yaml
var Resources embed.FS
