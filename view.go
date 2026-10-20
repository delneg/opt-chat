package main

import (
	"cmp"
	"container/heap"
	"slices"

	"github.com/samber/lo"
)

// view is a list of tree nodes tiling the whole chat, oldest first. It grows
// one line per message, and once past hi, merges the most due pairs down to lo
// in one batch: between batches it only grows at its end, so it stays cached.
type view struct {
	keys      []key
	lo, hi    int
	shrinking bool // a batch that could not reach lo yet, for want of built parents
	bytes     int  // its size: see cost
}

// cost is line k's bytes in a view: none until built, as in UniiChat, so a
// backlog's placeholders never make a view merge more than its lines need.
func cost(c *Chat, k key) int {
	if !c.built(k) {
		return 0
	}
	return len(c.line(k)) + 1
}

// push appends a message's line and reports whether the view merged.
func (v *view) push(k key, c *Chat) bool {
	v.keys = append(v.keys, k)
	v.bytes += cost(c, k)
	return v.check(c)
}

// recount measures the view anew, once its keys are set.
func (v *view) recount(c *Chat) { v.bytes = lo.SumBy(v.keys, func(k key) int { return cost(c, k) }) }

// built counts line k, built now, if the view holds it.
func (v *view) built(c *Chat, k key) {
	j, ok := slices.BinarySearchFunc(v.keys, k.start(), func(x key, s int) int { return cmp.Compare(x.start(), s) })
	if ok && v.keys[j] == k {
		v.bytes += cost(c, k)
	}
}

// check starts a batch once the view is past hi, goes on with one cut short,
// and reports whether the view merged.
func (v *view) check(c *Chat) bool {
	if v.bytes > v.hi {
		v.shrinking = true
	}
	return v.fit(c)
}

func (v *view) size() int { return v.bytes }

// end is the message after the view's last line.
func (v *view) end() int {
	if len(v.keys) == 0 {
		return 0
	}
	return v.keys[len(v.keys)-1].end()
}

func (v *view) fit(c *Chat) bool {
	if !v.shrinking {
		return false
	}
	merged := false
	for v.bytes > v.lo {
		j := mostDue(v.keys, v.end(), c.built)
		if j < 0 {
			return merged // the rest merges as parents get built
		}
		p := v.keys[j].parent()
		v.bytes += cost(c, p) - cost(c, v.keys[j]) - cost(c, v.keys[j+1])
		v.keys = slices.Replace(v.keys, j, j+2, p)
		merged = true
	}
	v.shrinking = false
	return merged
}

// mostDue returns j such that keys[j], keys[j+1] is the sibling pair that
// ended longest ago, measured in its own line size; the oldest of equal
// pairs. -1 if there is none, or if its parent is not built yet: a batch
// waits for it rather than merge a newer pair in its place, which during an
// import merged the chat's latest lines while the import's waited for their
// parents. This is Taelin's rollback push order (see view_test.go).
func mostDue(keys []key, T int, built func(key) bool) int {
	best, bestDue := -1, 0.0
	for j := 0; j+1 < len(keys); j++ {
		if a, b := keys[j], keys[j+1]; sibling(a, b) {
			if d := due(b, T); best < 0 || d > bestDue {
				best, bestDue = j, d
			}
		}
	}
	if best >= 0 && !built(keys[best].parent()) {
		return -1
	}
	return best
}

func sibling(a, b key) bool { return a.L == b.L && a.I%2 == 0 && b.I == a.I+1 }

// due is how long ago the pair ending with b ended, in its lines' size, T
// the view's end. Exact in float64: integers over a power of two.
func due(b key, T int) float64 { return float64(T-(b.end()-1)) / float64(int(1)<<b.L) }

// merged is keys with sibling pairs that ok allows merged, most due first as
// mostDue picks them, until their size, size now and measured by weigh, is
// at most lo; and that size. A heap of pairs over the keys as a linked list,
// as UniiChat merges (Memory.ts merge): a view of thousands of lines merges
// in milliseconds.
func merged(keys []key, T int, ok func(a, b key) bool, weigh func(key) int, size, lo int) ([]key, int) {
	ks := slices.Clone(keys)
	next := make([]int, len(ks))
	prev := make([]int, len(ks))
	for j := range ks {
		next[j], prev[j] = j+1, j-1
	}
	gone := make([]bool, len(ks))
	h := &pairs{}
	offer := func(j int) {
		if j < 0 || next[j] >= len(ks) {
			return
		}
		if a, b := ks[j], ks[next[j]]; sibling(a, b) && ok(a, b) {
			heap.Push(h, pair{due(b, T), j, a, b})
		}
	}
	for j := range ks {
		offer(j)
	}
	for h.Len() > 0 && size > lo {
		p := heap.Pop(h).(pair)
		j, n := p.j, next[p.j]
		if gone[j] || n >= len(ks) || ks[j] != p.a || ks[n] != p.b {
			continue // merged since
		}
		up := p.a.parent()
		size += weigh(up) - weigh(p.a) - weigh(p.b)
		ks[j], gone[n] = up, true
		next[j] = next[n]
		if next[n] < len(ks) {
			prev[next[n]] = j
		}
		offer(prev[j])
		offer(j)
	}
	out := make([]key, 0, len(ks))
	for j, k := range ks {
		if !gone[j] {
			out = append(out, k)
		}
	}
	return out, size
}

type pair struct {
	due  float64
	j    int // a's place
	a, b key
}

// pairs is a heap, most due first, the oldest of equals.
type pairs []pair

func (h pairs) Len() int      { return len(h) }
func (h pairs) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h pairs) Less(i, j int) bool {
	return h[i].due > h[j].due || h[i].due == h[j].due && h[i].j < h[j].j
}
func (h *pairs) Push(x any) { *h = append(*h, x.(pair)) }
func (h *pairs) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
