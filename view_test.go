package main

import (
	"slices"
	"testing"
)

// The gist's claim: with the push list's length as the budget, mostDue picks
// exactly the merges Taelin's rollback push makes, at every step.
func TestMostDueMatchesPush(t *testing.T) {
	type entry struct {
		keep  bool
		state int
	}
	var push func(s int, list []entry) []entry // newest first
	push = func(s int, list []entry) []entry {
		if len(list) == 0 {
			return []entry{{state: s}}
		}
		if !list[0].keep {
			list[0].keep = true
			return list
		}
		return append([]entry{{state: s}}, push(list[0].state, list[1:])...)
	}
	always := func(key) bool { return true }
	var list []entry
	var keys []key
	for tick := range 20_001 {
		list = push(tick, list)
		keys = append(keys, key{0, tick})
		for len(keys) > len(list) {
			j := mostDue(keys, tick+1, always)
			if j < 0 {
				t.Fatalf("t=%d: nothing to merge", tick)
			}
			keys = slices.Replace(keys, j, j+2, keys[j].parent())
		}
		var starts []int
		for _, e := range list {
			starts = append(starts, e.state)
		}
		slices.Sort(starts)
		for j, k := range keys {
			if k.start() != starts[j] {
				t.Fatalf("t=%d: view %v, push states %v", tick, keys, starts)
			}
		}
	}
}

func TestSplitAndClip(t *testing.T) {
	s := "aé" + string(make([]byte, 10)) + "€z"
	for _, p := range splitText(s, 3) {
		if len(p) > 3 || !utf8Start(p[0]) {
			t.Fatalf("bad piece %q", p)
		}
	}
	if got := clip("0123456789", 4); got != "01\n[... 6 bytes cut ...]\n89" {
		t.Fatalf("clip: %q", got)
	}
}
