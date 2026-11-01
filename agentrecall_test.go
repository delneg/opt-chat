package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/samber/lo"
	"regexp"
	"strings"
	"testing"
)

var recallAgentHit = regexp.MustCompile(`(?m)^\[(\w+)\]:(\d+)\+1\|.*zoom\("(\w+):(\d+)", 1\)$`)

// A word only a subagent's chat holds reaches a turn's recall, as a line
// named for the subagent that ends with the zoom opening its step; the
// main chat's lines come first and stay as they were.
func TestRecallReachesAgents(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	c.Log("user", "add a widget guard "+strings.Repeat("and more words ", 80))
	for range 6 {
		c.Log("opt", "something else entirely")
	}
	agentLog(t, c, "Locker", "task: add a guard\n\non: opus medium\n\n"+
		"tool@1761134400001#toolu_1: Write {\"file_path\":\"WidgetGuard.kt\"}\n\n"+
		"echo@1761134400002#toolu_1: wrote WidgetGuard.kt\n\n"+
		"agent@1761134400003: [Locker] done, see WidgetGuard\n\n")
	off := (&Agent{chat: c}).Recall("which file holds the guard? widgetguard", 7, 8)

	got := a.Recall("which file holds the guard? widgetguard", 7, 8)
	hits := recallAgentHit.FindAllStringSubmatch(got, -1)
	if len(hits) == 0 {
		t.Fatalf("no subagent line in recall: %s", got)
	}
	for _, h := range hits {
		if h[1] != "Locker" || h[3] != "Locker" || h[2] != h[4] {
			t.Errorf("line and zoom disagree: %s", h[0])
		}
		if step := zoomTool(a, h[3]+":"+h[4], 1); !strings.Contains(step, "WidgetGuard") {
			t.Errorf("zoom(%q, 1) opens a step without the word: %.120q", h[3]+":"+h[4], step)
		}
		if strings.Contains(h[0], "[Locker] done") {
			t.Errorf("the report, which the main chat holds, came back: %s", h[0])
		}
	}
	main, _, _ := strings.Cut(off, "</recall>")
	if !strings.Contains(got, main[strings.Index(main, "<recall>\n"):]) {
		t.Errorf("the main chat's lines changed:\n%s\nwant them as in:\n%s", got, off)
	}
}

// Many subagents full of the word add only a few short lines.
func TestRecallAgentsBounded(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	c.Log("user", "find the needle "+strings.Repeat("hay ", 400))
	for i := range 40 {
		var b strings.Builder
		b.WriteString("task: find it\n\n")
		for j := range 30 {
			word := "hay"
			if j == 3 {
				word = "needle"
			}
			fmt.Fprintf(&b, "echo@%d: %s %d %s\n\n", 1761134400000+j, word, j, strings.Repeat("z", 3000))
		}
		agentLog(t, c, fmt.Sprintf("A%02d", i), b.String())
	}
	off := (&Agent{chat: c}).Recall("where is the needle", 1, 8)
	got := a.Recall("where is the needle", 1, 8)
	if n := len(recallAgentHit.FindAllString(got, -1)); n == 0 || n > recallAgentMax {
		t.Fatalf("%d subagent lines, want 1 to %d: %s", n, recallAgentMax, got)
	}
	if grew := len(got) - len(off); grew > recallAgentMax*(recallAgentCut+120)+200 {
		t.Errorf("recall grew %d bytes", grew)
	}
}

// A step written after a recall read the log reaches the next recall: the
// parsed log is kept only while the log is unchanged.
func TestRecallAgentsSeesNewSteps(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	c.Log("user", "start "+strings.Repeat("and more words ", 80))
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("W", "task", "look around")
	a.agentWrite("W", "echo", "nothing yet")
	a.agentWrite("W", "agent", "still looking")
	if got := a.Recall("where is the walrus", 1, 8); strings.Contains(got, "walrus") {
		t.Fatalf("recall before the step: %s", got)
	}
	a.agentWrite("W", "echo", "the walrus is in the attic")
	a.agentWrite("W", "agent", "still looking")
	if got := a.Recall("where is the walrus", 1, 8); !strings.Contains(got, `zoom("W:3", 1)`) {
		t.Fatalf("recall after the step: %s", got)
	}
}

// With -agent-memory=false recall is the main chat's alone.
func TestRecallAgentsOff(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	c.Log("user", "fix the needle bug "+strings.Repeat("and more words ", 80))
	agentLog(t, c, "Fixer", "task: fix it\n\necho@1761134400003: found the needle\n\n")
	if got := a.Recall("needle", 1, 8); strings.Contains(got, "[Fixer]") || got != (&Agent{chat: c, mems: nil}).Recall("needle", 1, 8) {
		t.Fatalf("recall with memory off: %s", got)
	}
}

// Recall's size and reach on a copy of a real chat, with and without the
// subagents' steps: OPT_RECALL_DIR is the copy (never a live chat), and
// OPT_RECALL_QUESTIONS JSON lines {"id", "q", "gold": [substrings]}, both
// kept out of git. It makes no model call.
//
//	OPT_RECALL_DIR=/path/copy OPT_RECALL_QUESTIONS=q.jsonl go test -run TestRecallRealSize -v .
func TestRecallRealSize(t *testing.T) {
	dir, qs := os.Getenv("OPT_RECALL_DIR"), os.Getenv("OPT_RECALL_QUESTIONS")
	if dir == "" || qs == "" {
		t.Skip("set OPT_RECALL_DIR and OPT_RECALL_QUESTIONS")
	}
	c, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(qs)
	if err != nil {
		t.Fatal(err)
	}
	on, off := &Agent{chat: c, mems: newAgentMems(c, 1)}, &Agent{chat: c}
	before := len(c.msgs)
	gold := func(s string, golds []string) bool {
		return slices.ContainsFunc(golds, func(g string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(g)) })
	}
	var offB, onB, offN, onN, n int
	for line := range strings.Lines(string(b)) {
		var q struct {
			ID   any      `json:"id"`
			Q    string   `json:"q"`
			Gold []string `json:"gold"`
		}
		if json.Unmarshal([]byte(line), &q) != nil {
			continue
		}
		start := time.Now()
		r0 := off.Recall(q.Q, before, 8)
		took0 := time.Since(start)
		start = time.Now()
		r1 := on.Recall(q.Q, before, 8)
		took := time.Since(start)
		n, offB, onB = n+1, offB+len(r0), onB+len(r1)
		g0, g1 := gold(r0, q.Gold), gold(r1, q.Gold)
		offN, onN = offN+lo.Ternary(g0, 1, 0), onN+lo.Ternary(g1, 1, 0)
		t.Logf("q%v: off %d B gold %v; on %d B (+%d) gold %v; %d subagent lines; %v off, %v on",
			q.ID, len(r0), g0, len(r1), len(r1)-len(r0), g1, len(recallAgentHit.FindAllString(r1, -1)), took0.Round(time.Millisecond), took.Round(time.Millisecond))
	}
	t.Logf("%d questions: off %d B, gold in %d; on %d B (+%.0f%%), gold in %d", n, offB, offN, onB, 100*float64(onB-offB)/float64(max(offB, 1)), onN)
}
