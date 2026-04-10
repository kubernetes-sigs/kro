// Package crds embeds the CRD manifests for the Graph controller.
package crds

import "embed"

// YAMLs contains the CRD YAML files for Graph and GraphRevision.
//
//go:embed kro.run_graphs.yaml internal.kro.run_graphrevisions.yaml
var YAMLs embed.FS
