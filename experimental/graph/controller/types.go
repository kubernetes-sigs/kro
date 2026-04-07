// types.go defines the Graph data model and spec parsing.
//
// Resource and GraphSpec are the parsed representations of a Graph's spec.
// Everything else in the package depends on these types; they don't depend
// on anything else in the package.
package graphcontroller

import "fmt"

// Resource is a parsed Graph resource entry.
type Resource struct {
	ID          string
	Template    map[string]any
	ExternalRef map[string]any
	ForEach     map[string]string
	IncludeWhen []string
	ReadyWhen   []string // CEL conditions; all must be true for the resource to be "ready"
}

// GraphSpec holds the parsed spec of a Graph object.
type GraphSpec struct {
	Resources []Resource
}

// AllIdentifiers returns every identifier that CEL expressions in this spec
// might reference: resource IDs and forEach iterator variable names.
// Used to build the CEL environment with all variables declared upfront.
func (s *GraphSpec) AllIdentifiers() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, res := range s.Resources {
		add(res.ID)
		for varName := range res.ForEach {
			add(varName)
		}
	}
	return ids
}

// AllExpressions returns every CEL expression string in the spec.
// This is the single source of truth for what expressions exist in a Graph.
// The compilation phase uses this to eagerly compile all programs before
// the reconcile loop touches any resources.
func (s *GraphSpec) AllExpressions() []string {
	seen := map[string]bool{}
	var exprs []string

	add := func(strs []string) {
		for _, s := range strs {
			// Extract ${...} expressions from each string
			pos := 0
			for {
				dollars, expr, start, _ := findExpr(s, pos)
				if start < 0 {
					break
				}
				pos = start + len(dollars) + len(expr) + 2
				if len(dollars) != 1 {
					continue // $${...} is deferred, not evaluated at this level
				}
				if !seen[expr] {
					seen[expr] = true
					exprs = append(exprs, expr)
				}
			}
		}
	}

	// Collect expressions from each resource
	for _, res := range s.Resources { // Template expressions
		var templateStrings []string
		collectStrings(res.Template, &templateStrings)
		add(templateStrings)

		// ExternalRef expressions
		var refStrings []string
		collectStrings(res.ExternalRef, &refStrings)
		add(refStrings)

		// ForEach collection expressions
		for _, v := range res.ForEach {
			add([]string{v})
		}

		// Condition expressions (includeWhen, readyWhen)
		add(res.IncludeWhen)
		add(res.ReadyWhen)
	}

	return exprs
}

// extractGraphSpec parses the spec from a Graph object.
func extractGraphSpec(graphObj map[string]any) (*GraphSpec, error) {
	spec, ok := graphObj["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing spec")
	}
	rawResources, ok := spec["resources"]
	if !ok {
		return nil, fmt.Errorf("missing spec.resources")
	}
	resources, err := parseResourceList(rawResources)
	if err != nil {
		return nil, err
	}

	return &GraphSpec{Resources: resources}, nil
}

// parseResourceList converts a raw resource list into Resources.
func parseResourceList(raw any) ([]Resource, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.resources is %T, want []any", raw)
	}

	var resources []Resource
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource[%d] is %T, want map", i, item)
		}

		res := Resource{}
		if id, ok := m["id"].(string); ok {
			res.ID = id
		}
		if tmpl, ok := m["template"].(map[string]any); ok {
			res.Template = tmpl
		}
		if extRef, ok := m["externalRef"].(map[string]any); ok {
			res.ExternalRef = extRef
		}
		if fe, ok := m["forEach"].(map[string]any); ok {
			res.ForEach = make(map[string]string)
			for k, v := range fe {
				if vs, ok := v.(string); ok {
					res.ForEach[k] = vs
				}
			}
		}
		if iw, ok := m["includeWhen"].([]any); ok {
			for _, expr := range iw {
				if s, ok := expr.(string); ok {
					res.IncludeWhen = append(res.IncludeWhen, s)
				}
			}
		}
		if rw, ok := m["readyWhen"].([]any); ok {
			for _, expr := range rw {
				if s, ok := expr.(string); ok {
					res.ReadyWhen = append(res.ReadyWhen, s)
				}
			}
		}
		resources = append(resources, res)
	}
	return resources, nil
}
