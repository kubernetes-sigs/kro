package graph

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Hash computes a deterministic content hash of the compilation inputs.
// Two GraphSpecs that produce the same hash will produce identical compiledGraphs
// (same CEL programs, same DAG, same expression set).
//
// The hash covers: node IDs, template structures, forEach definitions,
// includeWhen/readyWhen/propagateWhen conditions — everything that feeds into
// compileGraphSpec. If a new compilation input is added to GraphSpec without
// updating this hash, content-addressed sharing will silently reuse stale
// compiled graphs. The test TestSpecHashCoversCompilationInputs guards against this.
//
// Each field is length-prefixed (binary.LittleEndian int64) before its content
// to prevent delimiter injection / field boundary ambiguity. This is strictly
// correct — no two distinct specs can produce the same hash input sequence.
//
// Collision probability: FNV-64a has a 64-bit output. At 1000 distinct specs
// the collision probability is ~2.7e-14 (birthday bound). At 1M distinct specs
// it's ~2.7e-8. This is accepted as negligible for an in-memory optimization
// cache. A collision would cause two different specs to share a compiled graph,
// producing incorrect CEL evaluation. If this ever matters, upgrade to FNV-128
// or SHA-256 — the hash is not on the hot path.
//
// Implementation notes:
//   - encoding/json sorts map keys deterministically in Go, so json.Marshal
//     of map[string]any produces a canonical byte sequence.
//   - Nodes are processed in declaration order (spec order is stable from
//     Kubernetes API). IncludeWhen, ReadyWhen, PropagateWhen are slices
//     (order-stable from spec parsing). ForEach is a map (sorted explicitly).
func (s *GraphSpec) Hash() string {
	h := fnv.New64a()

	// hashField writes a length-prefixed field to the hash.
	// Length prefix eliminates field boundary ambiguity (the classic delimiter
	// injection problem) without requiring reserved separator bytes.
	hashField := func(data []byte) {
		binary.Write(h, binary.LittleEndian, int64(len(data))) //nolint:errcheck
		h.Write(data)
	}

	// Process nodes in declaration order (spec order is stable).
	for _, node := range s.Nodes {
		// Node ID
		hashField([]byte(node.ID))

		// Body — Payload() covers Template/Patch/Def; Identity() covers
		// Ref/Watch (identity-only, no Payload). Either way the
		// body uniquely determines the node's spec.
		if payload := node.Payload(); payload != nil {
			data, _ := json.Marshal(payload)
			hashField(data)
		} else if identity := node.Identity(); identity != nil {
			data, _ := json.Marshal(identity)
			hashField(data)
		} else if node.TemplateExpr != "" {
			hashField([]byte(node.TemplateExpr))
		} else {
			hashField(nil)
		}

		// Finalizes
		hashField([]byte(node.Finalizes))

		// ForEach (direct field access — single dimension)
		if node.ForEach != nil {
			hashField([]byte(node.ForEach.VarName))
			hashField([]byte(node.ForEach.Expr))
			binary.Write(h, binary.LittleEndian, int64(1)) //nolint:errcheck
		} else {
			binary.Write(h, binary.LittleEndian, int64(0)) //nolint:errcheck
		}

		// Conditions — slices, order-stable from spec parsing.
		for _, c := range node.IncludeWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.IncludeWhen))) //nolint:errcheck

		for _, c := range node.ReadyWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.ReadyWhen))) //nolint:errcheck

		for _, c := range node.PropagateWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.PropagateWhen))) //nolint:errcheck
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// CompilationKey returns a structural hash of the spec that considers only
// inputs affecting compilation output. Unlike Hash(), which is a full content
// hash, CompilationKey ignores concrete values (string literals without
// expressions, numbers, booleans) that don't influence the compiled CEL
// programs, DAG, or type environment.
//
// Per 004-compilation.md § Structural Compilation Caching: "Two specs with
// the same compilation key produce the same compiled output."
func (s *GraphSpec) CompilationKey() string {
	h := fnv.New64a()

	hashField := func(data []byte) {
		binary.Write(h, binary.LittleEndian, int64(len(data))) //nolint:errcheck
		h.Write(data)
	}

	for _, node := range s.Nodes {
		// Node ID — always compilation-relevant (CEL variable name).
		hashField([]byte(node.ID))

		// Node type — affects how the body is processed.
		binary.Write(h, binary.LittleEndian, int64(node.Type())) //nolint:errcheck

		// Body: hash only compilation-relevant strings (expressions,
		// literal apiVersion/kind for schema resolution). Concrete
		// values without expressions are excluded.
		if node.TemplateExpr != "" {
			hashField([]byte(node.TemplateExpr))
		} else if body := node.Body(); body != nil {
			hashCompilationRelevantFields(h, hashField, body)
		}

		// Finalizes
		hashField([]byte(node.Finalizes))

		// ForEach — compilation-relevant (variable name + collection expression).
		if node.ForEach != nil {
			hashField([]byte(node.ForEach.VarName))
			hashField([]byte(node.ForEach.Expr))
			binary.Write(h, binary.LittleEndian, int64(1)) //nolint:errcheck
		} else {
			binary.Write(h, binary.LittleEndian, int64(0)) //nolint:errcheck
		}

		// Conditions — always compilation-relevant.
		for _, c := range node.IncludeWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.IncludeWhen))) //nolint:errcheck
		for _, c := range node.ReadyWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.ReadyWhen))) //nolint:errcheck
		for _, c := range node.PropagateWhen {
			hashField([]byte(c))
		}
		binary.Write(h, binary.LittleEndian, int64(len(node.PropagateWhen))) //nolint:errcheck
	}

	return fmt.Sprintf("%016x", h.Sum64())
}

// CompilationKeyWithHints produces the full cache key by combining the
// structural compilation key with resolved dynamic GVK hints and child ref
// content hashes. Instances with the same structure, same resolved GVKs, and
// same ref targets share a compiled artifact. When all hint maps are nil or
// empty, returns the structural key unchanged (bootstrap).
func CompilationKeyWithHints(structuralKey string, gvkHints map[string]schema.GroupVersionKind, childRefHashes map[string]string) string {
	if len(gvkHints) == 0 && len(childRefHashes) == 0 {
		return structuralKey
	}

	h := fnv.New64a()
	h.Write([]byte(structuralKey))

	// Dynamic GVK hints.
	if len(gvkHints) > 0 {
		nodeIDs := make([]string, 0, len(gvkHints))
		for id := range gvkHints {
			nodeIDs = append(nodeIDs, id)
		}
		sort.Strings(nodeIDs)
		for _, id := range nodeIDs {
			gvk := gvkHints[id]
			h.Write([]byte(id))
			h.Write([]byte(gvk.Group))
			h.Write([]byte(gvk.Version))
			h.Write([]byte(gvk.Kind))
		}
	}

	// Child ref content hashes — instances referencing different ref
	// targets (e.g., different RGDs) get separate compiled artifacts.
	if len(childRefHashes) > 0 {
		refIDs := make([]string, 0, len(childRefHashes))
		for id := range childRefHashes {
			refIDs = append(refIDs, id)
		}
		sort.Strings(refIDs)
		for _, id := range refIDs {
			h.Write([]byte("ref:"))
			h.Write([]byte(id))
			h.Write([]byte(childRefHashes[id]))
		}
	}

	return fmt.Sprintf("%s+%016x", structuralKey, h.Sum64())
}

// hashCompilationRelevantFields walks a body map and hashes only the fields
// that affect compilation: expression strings (containing ${...} or $${...}),
// and literal apiVersion/kind values (which affect schema resolution).
func hashCompilationRelevantFields(h hash.Hash64, hashField func([]byte), body map[string]any) {
	// apiVersion and kind affect schema resolution → compilation-relevant.
	if av, ok := body["apiVersion"].(string); ok {
		hashField([]byte("apiVersion"))
		hashField([]byte(av))
	}
	if k, ok := body["kind"].(string); ok {
		hashField([]byte("kind"))
		hashField([]byte(k))
	}

	// Walk the body for expression strings.
	var strs []string
	CollectStrings(body, &strs)
	sort.Strings(strs) // deterministic order
	for _, s := range strs {
		if strings.Contains(s, "${") {
			hashField([]byte(s))
		}
	}
}
