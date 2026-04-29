// Package crds embeds the chart CRD YAML files for Graph and GraphRevision.
package crds

import "embed"

//go:embed *.yaml
var FS embed.FS
