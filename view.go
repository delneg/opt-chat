package main

import (
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
}

// push appends a message's line and reports whether the view merged.
func (v *view) push(k key, c *Chat) bool {
	v.keys = append(v.keys, k)
	return v.check(c)
}

// check starts a batch once the view is past hi, goes on with one cut short,
// and reports whether the view merged.
func (v *view) check(c *Chat) bool {
	if v.size(c) > v.hi {
		v.shrinking = true
	}
	return v.fit(c)
}

func (v *view) size(c *Chat) int {
	return lo.SumBy(v.keys, func(k key) int { return len(c.line(k)) + 1 })
}

func (v *view) fit(c *Chat) bool {
	if !v.shrinking {
		return false
	}
	merged := false
	for size := v.size(c); size > v.lo; {
		j := mostDue(v.keys, len(c.msgs), c.built)
		if j < 0 {
			return merged // the rest merges at the next messages, as parents get built
		}
		p := v.keys[j].parent()
		size += len(c.line(p)) - len(c.line(v.keys[j])) - len(c.line(v.keys[j+1])) - 1
		v.keys = slices.Replace(v.keys, j, j+2, p)
		merged = true
	}
	v.shrinking = false
	return merged
}

// mostDue returns j such that keys[j], keys[j+1] is the sibling pair with a
// built parent that ended longest ago, measured in its own line size; the
// oldest of equal pairs. -1 if there is none. This is Taelin's rollback push
// order (see view_test.go).
func mostDue(keys []key, T int, built func(key) bool) int {
	best, bestDue := -1, 0.0
	for j := 0; j+1 < len(keys); j++ {
		a, b := keys[j], keys[j+1]
		if a.L != b.L || a.I%2 != 0 || b.I != a.I+1 || !built(a.parent()) {
			continue
		}
		// Exact in float64: integers over a power of two.
		if due := float64(T-(b.end()-1)) / float64(int(1)<<a.L); best < 0 || due > bestDue {
			best, bestDue = j, due
		}
	}
	return best
}
