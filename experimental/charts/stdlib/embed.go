// Package chartstdlib embeds the stdlib helm chart templates. The chart
// is the canonical home for these resources; the stdlib package imports
// this FS for test bootstrap.
package chartstdlib

import "embed"

// Templates contains the stdlib chart template files.
//
//go:embed templates/*.yaml
var Templates embed.FS
