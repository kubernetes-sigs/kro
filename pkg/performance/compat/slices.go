package compat

// ContainsFunc reports whether any element e of slice satisfies f(e).
func ContainsFunc[E any](s []E, f func(E) bool) bool {
	for _, v := range s {
		if f(v) {
			return true
		}
	}
	return false
}

// Clone returns a copy of the slice.
// The elements are copied using assignment, so this is a shallow clone.
func Clone[S ~[]E, E any](s S) S {
	// Preserve nil in case it matters.
	if s == nil {
		return nil
	}
	result := make(S, len(s))
	copy(result, s)
	return result
}

// Sort sorts a slice of any ordered type in ascending order.
func Sort[S ~[]E, E Ordered](x S) {
	n := len(x)
	quickSort(x, 0, n, maxDepth(n))
}

type Ordered interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr |
		~float32 | ~float64 |
		~string
}

// quickSort is a quicksort implementation.
func quickSort[S ~[]E, E Ordered](data S, a, b, maxDepth int) {
	for b-a > 1 {
		if maxDepth == 0 {
			heapSort(data, a, b)
			return
		}
		maxDepth--
		mlo, mhi := doPivot(data, a, b)
		if mlo-a < b-mhi {
			quickSort(data, a, mlo, maxDepth)
			a = mhi
		} else {
			quickSort(data, mhi, b, maxDepth)
			b = mlo
		}
	}
}

// maxDepth returns a reasonable maximum depth for quicksort.
func maxDepth(n int) int {
	var depth int
	for i := n; i > 0; i >>= 1 {
		depth++
	}
	return depth * 2
}

// doPivot does the partitioning for quicksort.
func doPivot[S ~[]E, E Ordered](data S, lo, hi int) (midlo, midhi int) {
	m := lo + (hi-lo)/2
	if hi-lo > 40 {
		s := (hi - lo) / 8
		medianOfThree(data, lo, lo+s, lo+2*s)
		medianOfThree(data, m, m-s, m+s)
		medianOfThree(data, hi-1, hi-1-s, hi-1-2*s)
	}
	medianOfThree(data, lo, m, hi-1)

	pivot := lo
	a, b, c, d := lo+1, lo+1, hi, hi

	for {
		for b < c {
			if data[b] < data[pivot] {
				b++
			} else if !(data[pivot] < data[b]) {
				data[a], data[b] = data[b], data[a]
				a++
				b++
			} else {
				break
			}
		}
		for b < c {
			if data[pivot] < data[c-1] {
				c--
			} else if !(data[c-1] < data[pivot]) {
				data[c-1], data[d-1] = data[d-1], data[c-1]
				c--
				d--
			} else {
				break
			}
		}
		if b >= c {
			break
		}
		data[b], data[c-1] = data[c-1], data[b]
		b++
		c--
	}

	n := min(b-a, a-lo)
	swapRange(data, lo, b-n, n)

	n = min(hi-d, d-c)
	swapRange(data, c, hi-n, n)

	return lo + b - a, hi - (d - c)
}

func medianOfThree[S ~[]E, E Ordered](data S, m1, m0, m2 int) {
	if data[m1] < data[m0] {
		data[m1], data[m0] = data[m0], data[m1]
	}
	if data[m2] < data[m1] {
		data[m2], data[m1] = data[m1], data[m2]
		if data[m1] < data[m0] {
			data[m1], data[m0] = data[m0], data[m1]
		}
	}
}

func swapRange[S ~[]E, E any](data S, a, b, n int) {
	for i := 0; i < n; i++ {
		data[a+i], data[b+i] = data[b+i], data[a+i]
	}
}

func heapSort[S ~[]E, E Ordered](data S, a, b int) {
	first := a
	lo := 0
	hi := b - a

	for i := (hi - 1) / 2; i >= 0; i-- {
		siftDown(data, i, hi, first)
	}

	for i := hi - 1; i >= 0; i-- {
		data[first], data[first+i] = data[first+i], data[first]
		siftDown(data, lo, i, first)
	}
}

func siftDown[S ~[]E, E Ordered](data S, lo, hi, first int) {
	root := lo
	for {
		child := 2*root + 1
		if child >= hi {
			break
		}
		if child+1 < hi && data[first+child] < data[first+child+1] {
			child++
		}
		if !(data[first+root] < data[first+child]) {
			return
		}
		data[first+root], data[first+child] = data[first+child], data[first+root]
		root = child
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}