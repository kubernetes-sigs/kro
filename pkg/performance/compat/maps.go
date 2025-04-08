package compat

// Clone returns a copy of the map.
// The elements are copied using assignment, so this is a shallow clone.
func Clone[M ~map[K]V, K comparable, V any](m M) M {
	if m == nil {
		return nil
	}
	result := make(M, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// Keys returns the keys of the map m in a slice.
func Keys[M ~map[K]V, K comparable, V any](m M) []K {
	r := make([]K, 0, len(m))
	for k := range m {
		r = append(r, k)
	}
	return r
}

// Values returns the values of the map m in a slice.
func Values[M ~map[K]V, K comparable, V any](m M) []V {
	r := make([]V, 0, len(m))
	for _, v := range m {
		r = append(r, v)
	}
	return r
}

// DeleteFunc deletes any key-value pairs from m for which f returns true.
func DeleteFunc[M ~map[K]V, K comparable, V any](m M, f func(K, V) bool) {
	for k, v := range m {
		if f(k, v) {
			delete(m, k)
		}
	}
}

// EqualFunc reports whether two maps contain the same key-value pairs
// according to the comparison function.
func EqualFunc[M1 ~map[K]V1, M2 ~map[K]V2, K comparable, V1, V2 any](m1 M1, m2 M2, eq func(V1, V2) bool) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v1 := range m1 {
		if v2, ok := m2[k]; !ok || !eq(v1, v2) {
			return false
		}
	}
	return true
}

// Equal reports whether two maps contain the same key-value pairs.
// Values are compared using ==.
func Equal[M1, M2 ~map[K]V, K, V comparable](m1 M1, m2 M2) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v1 := range m1 {
		if v2, ok := m2[k]; !ok || v1 != v2 {
			return false
		}
	}
	return true
}