// Package deploy contains deployment manifests for the Graph controller
// and embeds CRD YAMLs for bootstrap mode.
package deploy

import "embed"

// CRDs contains the CRD YAML files for Graph and GraphRevision.
// Only CRD files — bootstrap.go applies everything embedded here as CRDs.
// All other CRDs (Kind, Decorator, Singleton) are created by the
// created by the standard library through the Graph controller's
// reconciliation loop.
//
//go:embed experimental.kro.run_graphs.yaml experimental.kro.run_graphrevisions.yaml
var CRDs embed.FS
