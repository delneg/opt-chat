package main

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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

// Text limits count characters (gist §1), the line limit bytes (§2).
func TestSplitAndClip(t *testing.T) {
	s := "aé" + string(make([]byte, 10)) + "€z"
	for _, p := range splitText(s, 3) {
		if utf8.RuneCountInString(p) > 3 || !utf8.ValidString(p) {
			t.Fatalf("bad piece %q", p)
		}
	}
	if strings.Join(splitText(s, 3), "") != s || len(splitText(s, 3)) != 5 {
		t.Fatalf("split: %q", splitText(s, 3))
	}
	long := strings.Repeat("é", 40_000)
	got := clip(long, clipMax)
	if n := utf8.RuneCountInString(got); n > clipMax || n < clipMax-2 || !strings.HasPrefix(got, "éé") || !strings.HasSuffix(got, "éé") ||
		!strings.Contains(got, "[... ") || !utf8.ValidString(got) || len(splitText(got, clipMax)) != 1 {
		t.Fatalf("clip: %d characters, %.20q...%.20q", n, got, got[len(got)-20:])
	}
	if clip("short", clipMax) != "short" || len(clip(long, 4)) == 0 {
		t.Fatal("clip edge cases")
	}
	if h := head("aé€", 4); h != "aé" {
		t.Fatalf("head = %q", h)
	}
}
