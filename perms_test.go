package main

import (
	"slices"
	"strings"
	"testing"
)

func asksOf(c *Chat) []ask {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	return slices.Clone(c.hub.asks)
}

// A tool call Claude Code asks about waits for the user: the pages show the
// question, a mode switch reaches the call underway, and yes-and-don't-ask-
// again allows it for this call and the next ones, written nowhere.
func TestPermission(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	rec := record(c)
	a, calls := fakeAgent(t, c)
	h := &harness{agent: a}
	t.Cleanup(h.quit)
	h.input(message{kind: "user", text: "ask-permission"})
	waitFor(t, "the question", func() bool { return len(asksOf(c)) == 1 })
	q := asksOf(c)[0]
	if q.Tool != "Bash" || string(q.Input) != `{"command":"rm x"}` || q.Desc != "Remove x" || q.Always != "Bash(rm *); files in /far" || q.Who != "" {
		t.Fatalf("the question: %+v", q)
	}
	if args := calls()[0]; flagOf(args, "--permission-prompt-tool") != "stdio" || flagOf(args, "--permission-mode") != "auto" {
		t.Fatalf("turn args: %q", args)
	}
	a.setMode("acceptEdits")
	if !a.answer(q.ID, true, true) || a.answer(q.ID, true, true) {
		t.Fatal("an answer: once")
	}
	waitFor(t, "the turn to end", h.idle)
	var reply string
	for _, m := range logged(c) {
		if strings.HasPrefix(m, "opt: modes") {
			reply = m
		}
	}
	if !strings.Contains(reply, "modes [acceptEdits]") || !strings.Contains(reply, `"behavior":"allow"`) ||
		!strings.Contains(reply, `"destination":"session"`) || !strings.Contains(reply, `"ruleContent":"rm *"`) {
		t.Fatalf("the call got: %q", reply)
	}
	if len(asksOf(c)) != 0 {
		t.Fatal("an answered question stayed")
	}

	// The next call starts in the mode, the rule allowed; a no stops the turn.
	h.input(message{kind: "user", text: "ask-permission again"})
	waitFor(t, "the question", func() bool { return len(asksOf(c)) == 1 })
	args := calls()[1]
	if flagOf(args, "--permission-mode") != "acceptEdits" || !strings.HasSuffix(flagOf(args, "--allowedTools"), ",Bash(rm *)") ||
		flagOf(args, "--add-dir") != "/far" {
		t.Fatalf("next turn's args: %q", args)
	}
	a.answer(asksOf(c)[0].ID, false, false)
	waitFor(t, "the turn to end", h.idle)
	if !slices.ContainsFunc(logged(c), func(m string) bool {
		return strings.Contains(m, `"behavior":"deny"`) && strings.Contains(m, `"interrupt":true`)
	}) {
		t.Fatalf("logged %q", logged(c))
	}

	if w := rec.warnings(); len(w) > 0 {
		t.Fatalf("a no warned: %q", w)
	}

	// Stopped while it asks: the question goes.
	h.input(message{kind: "user", text: "ask-permission a third time"})
	waitFor(t, "the question", func() bool { return len(asksOf(c)) == 1 })
	h.stop()
	waitFor(t, "the question to go", func() bool { return len(asksOf(c)) == 0 })
	waitFor(t, "the turn to end", h.idle)

	// Taken back by Claude Code: the question goes.
	h.input(message{kind: "user", text: "ask-then-cancel"})
	waitFor(t, "the question", func() bool { return len(asksOf(c)) == 1 })
	waitFor(t, "the question to go", func() bool { return len(asksOf(c)) == 0 })
	waitFor(t, "the turn to end", h.idle)
}

func TestModes(t *testing.T) {
	t.Parallel()
	for _, step := range [][2]string{{"manual", "acceptEdits"}, {"acceptEdits", "auto"}, {"auto", "manual"}, {"bypassPermissions", "manual"}, {"default", "acceptEdits"}} {
		if got := nextMode(step[0]); got != step[1] {
			t.Fatalf("after %s: %s", step[0], got)
		}
	}
	if validMode("plan") || !validMode("acceptEdits") {
		t.Fatal("validMode")
	}
}
