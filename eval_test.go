package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// -eval asks each question as a side question with none before it, and
// writes it back with its answer, what recall gave it and what the call
// did; nothing joins the log or btw/.
func TestEval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, _ := fakeChat(t, dir, 100)
	a, _ := fakeAgent(t, c)
	a.recall = 8
	c.Log("user", "the wombat lives in the shed "+strings.Repeat("filler ", 100)) // over nodeMax: not shown whole, so recalled
	for i := range 10 {
		c.Log("opt", fmt.Sprintf("noted %d", i))
	}
	waitIdle(t, c)
	in, out := filepath.Join(dir, "q.jsonl"), filepath.Join(dir, "a.jsonl")
	qs := `{"id":"q1","q":"side-ask where does the wombat live?","gold":"the shed"}` + "\n\n" + `{"id":"q2","q":"side-fail"}` + "\n"
	if err := os.WriteFile(in, []byte(qs), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runEval(a, in, out, 2); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		got[r["id"].(string)] = r
	}
	q1, q2 := got["q1"], got["q2"]
	if q1["gold"] != "the shed" || !strings.Contains(q1["answer"].(string), "where does the wombat live?") || q1["err"] != nil ||
		!slices.Contains(q1["recalled"].([]any), any(float64(0))) || strings.Contains(q1["answer"].(string), "<btw>") {
		t.Fatalf("q1: %v", q1)
	}
	if q2["err"] != "overloaded" {
		t.Fatalf("q2: %v", q2)
	}
	if len(c.msgs) != 11 || len(c.asides) != 0 {
		t.Fatalf("log %d, asides %d", len(c.msgs), len(c.asides))
	}
	if err := os.WriteFile(in, []byte(`{"id":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runEval(a, in, out, 1); err == nil || !strings.Contains(err.Error(), `a line has no "q"`) {
		t.Fatal(err)
	}
}
