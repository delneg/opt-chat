package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A log's entries, a text's own blank lines in it, and a text whose blank
// line is followed by what looks like an entry: taken for one.
func TestParseSteps(t *testing.T) {
	t.Parallel()
	log := "task: do it\n\nwell\n\non: opus high\n\ntool: Bash {\"command\":\"ls\"}\n\necho: a\n\nb\n\nagent: done:\nyes\n\n"
	want := []step{{Kind: "task", Text: "do it\n\nwell"}, {Kind: "on", Text: "opus high"}, {Kind: "tool", Text: `Bash {"command":"ls"}`}, {Kind: "echo", Text: "a\n\nb"}, {Kind: "agent", Text: "done:\nyes"}}
	if got := parseSteps([]byte(log)); !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := parseSteps(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
	if got := plainSteps([]byte(log)); string(got) != log {
		t.Fatalf("an old log, plain: %q", got)
	}
}

// Steps written with their times and ids read back with them, a log that
// is half old entries and half new reads whole, and what Opt reads of it
// is the log as it was before times were kept.
func TestStepTimes(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	if err := os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{chat: c}
	path := a.agentLog("Timed")
	old := "task: look\n\non: opus high\n\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	before := nowMs()
	a.agentWrite("Timed", "agent", "two calls at once")
	a.agentWriteRef("Timed", "tool", "Bash {\"command\":\"sleep 2\"}", "toolu_01A")
	a.agentWriteRef("Timed", "tool", "Read {\"file_path\":\"/Users/me/x\"}", "toolu_01B")
	a.agentWriteRef("Timed", "echo", "x\n\ny", "toolu_01B")
	a.agentWriteRef("Timed", "echo", "", "toolu_01A")
	a.agentWriteRef("Timed", "echo", "no id", "")
	a.agentWriteRef("Timed", "echo", "a spaced id", "a b")
	after := nowMs()

	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), old+"agent@") || !strings.Contains(string(b), "#toolu_01A: Bash") {
		t.Fatalf("log: %q", b)
	}
	got, cut, err := a.steps("Timed")
	if err != nil || cut || len(got) != 9 {
		t.Fatalf("steps %v %v: %+v", cut, err, got)
	}
	if got[0] != (step{Kind: "task", Text: "look"}) || got[1] != (step{Kind: "on", Text: "opus high"}) {
		t.Fatalf("old entries: %+v", got[:2])
	}
	refs := []string{"", "toolu_01A", "toolu_01B", "toolu_01B", "toolu_01A", "", ""}
	texts := []string{"two calls at once", "Bash {\"command\":\"sleep 2\"}", "Read {\"file_path\":\"/Users/me/x\"}", "x\n\ny", "", "no id", "a spaced id"}
	for j, s := range got[2:] {
		if s.At < before || s.At > after || s.Ref != refs[j] || s.Text != texts[j] {
			t.Fatalf("step %d: %+v", j+2, s)
		}
	}

	want := old + "agent: two calls at once\n\ntool: Bash {\"command\":\"sleep 2\"}\n\ntool: Read {\"file_path\":\"/Users/me/x\"}\n\n" +
		"echo: x\n\ny\n\necho: \n\necho: no id\n\necho: a spaced id\n\n"
	if z := agentPlain(c.dir, "Timed"); z != want {
		t.Fatalf("zoom: %q", z)
	}
}

// A head's text that looks like a timed head stays text when it is not
// after a blank line.
func TestStepHeads(t *testing.T) {
	t.Parallel()
	log := "echo@5#t1: a\ntool@6#t2: inside\n\ntool@7: b\n\n"
	want := []step{{Kind: "echo", Text: "a\ntool@6#t2: inside", At: 5, Ref: "t1"}, {Kind: "tool", Text: "b", At: 7}}
	if got := parseSteps([]byte(log)); !slices.Equal(got, want) {
		t.Fatalf("%+v", got)
	}
	if got := string(plainSteps([]byte(log))); got != "echo: a\ntool@6#t2: inside\n\ntool: b\n\n" {
		t.Fatalf("plain: %q", got)
	}
	if h := stepHead(step{Kind: "tool", At: 9, Ref: "functions.Bash:0"}); h != "tool@9#functions.Bash:0: " {
		t.Fatal(h)
	}
	if got := parseSteps([]byte("tool@9#functions.Bash:0: Bash x\n\n")); len(got) != 1 || got[0].Ref != "functions.Bash:0" || got[0].Text != "Bash x" {
		t.Fatalf("an id with a colon: %+v", got)
	}
}

// A subagent's thinking is in its log, for the page, and out of what Opt
// reads of it, wherever it is.
func TestThinkingSteps(t *testing.T) {
	t.Parallel()
	log := "thinking@1: first\n\nmore\n\ntool@2#t1: Bash x\n\nthinking@3: then\n\necho@4#t1: out\n\nthinking@5: last\n\n"
	if got := parseSteps([]byte(log)); len(got) != 5 || got[0] != (step{Kind: "thinking", Text: "first\n\nmore", At: 1}) {
		t.Fatalf("%+v", got)
	}
	if got := string(plainSteps([]byte(log))); got != "tool: Bash x\n\necho: out\n\n" {
		t.Fatalf("plain: %q", got)
	}
}
