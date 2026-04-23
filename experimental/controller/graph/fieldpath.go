// fieldpath.go defines the FieldPath type and utility functions for working
// with field paths extracted from CEL expressions.
//
// A FieldPath is the deepest static prefix of a dot-select chain in a CEL
// expression. Static means no function calls, index operations, or
// comprehensions intervene. Examples:
//
//	deploy.status.availableReplicas      → ["status", "availableReplicas"]
//	deploy.metadata.labels["app"]        → ["metadata", "labels"]
//	deploy.status.conditions.filter(...) → ["status", "conditions"]
//	has(deploy.status.conditions)        → ["status", "conditions"]
//
// The CEL AST walking that produces FieldPaths lives in compiler/fieldpath.go.
package graph

// FieldPath is a sequence of field names representing a path into an object.
// For example, ["status", "availableReplicas"] represents .status.availableReplicas.
type FieldPath []string

// Equal returns true if two FieldPaths have the same segments.
func (fp FieldPath) Equal(other FieldPath) bool {
	if len(fp) != len(other) {
		return false
	}
	for i := range fp {
		if fp[i] != other[i] {
			return false
		}
	}
	return true
}

// AddFieldPath appends a FieldPath to a slice, deduplicating exact matches.
func AddFieldPath(paths *[]FieldPath, path FieldPath) {
	for _, existing := range *paths {
		if existing.Equal(path) {
			return
		}
	}
	*paths = append(*paths, path)
}

// AddPath adds a FieldPath to the result map, deduplicating exact matches.
func AddPath(result map[string][]FieldPath, root string, path FieldPath) {
	for _, existing := range result[root] {
		if existing.Equal(path) {
			return
		}
	}
	result[root] = append(result[root], path)
}
