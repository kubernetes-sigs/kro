// Package deploy contains deployment manifests for the Graph controller
// and embeds CRD YAMLs for bootstrap mode.
package deploy

import "embed"

// CRDs contains the CRD YAML files for Graph and GraphRevision.
// Only CRD files — bootstrap.go applies everything embedded here as CRDs.
//
//go:embed kro.run_graphs.yaml internal.kro.run_graphrevisions.yaml
var CRDs embed.FS
