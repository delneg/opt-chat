package main

import (
	"slices"
	"testing"
)

// A log's entries, a text's own blank lines in it, and a text whose blank
// line is followed by what looks like an entry: taken for one.
func TestParseSteps(t *testing.T) {
	log := "task: do it\n\nwell\n\non: opus high\n\ntool: Bash {\"command\":\"ls\"}\n\necho: a\n\nb\n\nagent: done:\nyes\n\n"
	want := []step{{"task", "do it\n\nwell"}, {"on", "opus high"}, {"tool", `Bash {"command":"ls"}`}, {"echo", "a\n\nb"}, {"agent", "done:\nyes"}}
	if got := parseSteps([]byte(log)); !slices.Equal(got, want) {
		t.Fatalf("%q", got)
	}
	if got := parseSteps(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty: %q", got)
	}
}
