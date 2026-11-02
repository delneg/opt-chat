package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A subagent of the user's own, from the page's menu: made blank, with no
// call until the first message, which starts it in its folder on its
// model, effort and window, the cap left out, with no view; Opt's chat
// hears who it is once, and gets no report of its replies.
func TestNewAgent(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	a.agentCap = 400_000
	proj := filepath.Join(t.TempDir(), "proj")
	if err := os.Mkdir(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct {
		dir, model, effort string
		window             int
	}{
		{proj, "big", "huge", 0},
		{proj, "big", "high", 50_000},
		{proj, "big", "high", 2_000_000},
		{proj, "two words", "high", 0},
		{filepath.Join(proj, "nope"), "big", "high", 0},
		{"proj", "big", "high", 0},
	} {
		if out, ok := a.NewAgent("", bad.dir, bad.model, bad.effort, bad.window); ok {
			t.Fatalf("%+v: made %q", bad, out)
		}
	}
	name, ok := a.NewAgent("", proj+"/", "big", "xhigh", 600_000)
	if !ok || name != "proj" {
		t.Fatalf("made %q, %v", name, ok)
	}
	if again, _ := a.NewAgent("", proj, "big", "high", 0); again != "proj-2" {
		t.Fatalf("the second: %q", again)
	}
	rec, err := a.loadRecord("proj")
	if err != nil || !rec.Blank || rec.State != "new" || rec.Dir != proj || rec.Window != 600_000 || rec.Session == "" {
		t.Fatalf("its record: %+v, %v", rec, err)
	}
	a.mu.Lock()
	s := *a.subs["proj"]
	a.mu.Unlock()
	if s.State != "new" || s.Dir != proj || !s.Blank || s.Model != "big" || s.Effort != "xhigh" {
		t.Fatalf("the rail's: %+v", s)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("%d calls before its first message", n)
	}

	if out, ok := a.TellUser("proj", "hi there", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its reply", func() bool { return agentState(a, "proj") == "resting" })
	all := calls()
	if len(all) != 1 {
		t.Fatalf("%d calls", len(all))
	}
	args := all[0]
	if flagOf(args, "--session-id") != rec.Session || flagOf(args, "--model") != "big" || flagOf(args, "--effort") != "xhigh" ||
		!slices.Contains(args, `{"autoCompactEnabled":true,"autoCompactWindow":633000}`) {
		t.Fatalf("its call: %q", args)
	}
	wd, _ := os.ReadFile(fakeArgsFile(a) + ".cwd")
	if real, _ := filepath.EvalSymlinks(proj); strings.TrimSpace(string(wd)) != real {
		t.Fatalf("it ran in %q, not %s", wd, real)
	}
	log := c.ZoomAgent("proj")
	if !strings.Contains(log, `agent: answer to "You are subagent [proj], the user's own`) || !strings.Contains(log, `from the page:\nhi there"`) ||
		strings.Contains(log, "<chat>") || strings.Contains(log, "task:") {
		t.Fatalf("its log: %q", log)
	}
	if lines := workLines(c, "[opt-chat] The user made subagent [proj]"); len(lines) != 1 || !strings.Contains(lines[0], "big xhigh, in "+proj+", autocompact past 600k tokens") {
		t.Fatalf("the chat's note: %q", lines)
	}
	if out, ok := a.TellUser("proj", "and again", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its second reply", func() bool {
		return strings.Contains(c.ZoomAgent("proj"), "and again\"") && agentState(a, "proj") == "resting"
	})
	if n := len(workLines(c, "[opt-chat] The user made")); n != 1 {
		t.Fatalf("%d notes of who it is", n)
	}
	if n := len(workLines(c, "[user → proj]")); n != 2 {
		t.Fatalf("%d lines of the user's messages", n)
	}
	if n := len(workLines(c, "[proj]")); n != 0 {
		t.Fatalf("%d reports to Opt of its replies to the user", n)
	}

	// Autocompact off: it never compacts.
	if out, ok := a.TellUser("proj-2", "hello", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "proj-2's reply", func() bool { return agentState(a, "proj-2") == "resting" })
	if args := calls()[1]; !slices.Contains(args, `{"autoCompactEnabled":false}`) {
		t.Fatalf("proj-2's call: %q", args)
	}
}

// The user's own show again after a restart, made and never told
// anything or talked to; cleared, they are gone for good.
func TestNewAgentKept(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	name, ok := a.NewAgent("Mine", t.TempDir(), "", "", 0)
	if !ok || name != "Mine" {
		t.Fatalf("made %q, %v", name, ok)
	}
	if rec, _ := a.loadRecord(name); rec.Model != "mid" || rec.Effort != a.aeffort {
		t.Fatalf("its defaults: %+v", rec)
	}
	dead, _ := a.NewAgent("Dead", t.TempDir(), "", "", 0) // ended, its session never saved: nothing to go on with
	a.note(dead, func(r *agentRecord) { r.State = "ended" })
	used, _ := a.NewAgent("Used", t.TempDir(), "", "", 0)
	if out, ok := a.TellUser(used, "hi", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its reply", func() bool { return agentState(a, used) == "resting" })
	a.quit()
	b, _ := fakeAgent(t, c)
	b.recoverAgents()
	if st, st2, st3 := agentState(b, name), agentState(b, used), agentState(b, dead); st != "new" || st2 != "ended" || st3 != "" {
		t.Fatalf("after a restart: %q, %q, %q", st, st2, st3)
	}
	if !b.ClearAgent(name) || !b.ClearAgent(used) || agentState(b, name) != "" {
		t.Fatal("not cleared")
	}
	if rec, _ := b.loadRecord(name); rec.State != "ended" || !rec.Cleared {
		t.Fatalf("its record: %+v", rec)
	}
	d, _ := fakeAgent(t, c)
	d.recoverAgents()
	if st, st2 := agentState(d, name), agentState(d, used); st != "" || st2 != "" {
		t.Fatalf("cleared, back after a restart: %q, %q", st, st2)
	}
	if out, ok := d.TellUser(used, "back", nil, false); !ok { // told again, it is back, after a restart too
		t.Fatal(out)
	}
	waitFor(t, "its reply", func() bool { return agentState(d, used) == "resting" })
	if rec, _ := d.loadRecord(used); rec.Cleared {
		t.Fatalf("its record: %+v", rec)
	}
}

// The page's newAgent: the menu's answer is the name made, or why none
// was, and the rail lists it; status gives the menu its defaults.
func TestWebNewAgent(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	dir := t.TempDir()
	ws.send(map[string]any{"t": "newAgent", "name": "Side", "dir": dir, "model": "big", "effort": "high", "window": 200_000})
	var made, listed bool // in either order: the status goes to every page
	ws.next("made, and the rail's", func(u map[string]any) bool {
		switch u["t"] {
		case "newAgent":
			if u["name"] != "Side" || u["error"] != nil {
				t.Fatalf("newAgent: %v", u)
			}
			made = true
		case "status":
			st := u["status"].(map[string]any)
			subs, _ := st["subs"].([]any)
			listed = listed || st["agentModel"] == wt.h.agent.amodel && len(subs) == 1 && subs[0].(map[string]any)["state"] == "new" && subs[0].(map[string]any)["dir"] == dir
		}
		return made && listed
	})
	ws.send(map[string]any{"t": "newAgent", "dir": filepath.Join(dir, "nope"), "model": "big", "effort": "high"})
	if u := ws.next("refused", typ("newAgent")); u["error"] != "no folder "+filepath.Join(dir, "nope") {
		t.Fatalf("newAgent: %v", u)
	}
}

// One whose first start failed before Claude Code saved its session stays
// new: the next message starts it afresh, not from a session that isn't
// there.
func TestNewAgentRetry(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	dir := filepath.Join(t.TempDir(), "gone")
	os.Mkdir(dir, 0o700)
	name, _ := a.NewAgent("Again", dir, "big", "high", 0)
	os.Remove(dir) // its call can't start there
	if out, ok := a.TellUser(name, "hi", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its failure", func() bool { return agentState(a, name) == "failed" })
	if rec, _ := a.loadRecord(name); rec.State != "new" {
		t.Fatalf("its record: %+v", rec)
	}
	os.Mkdir(dir, 0o700)
	if out, ok := a.TellUser(name, "hi again", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its reply", func() bool { return agentState(a, name) == "resting" })
	rec, _ := a.loadRecord(name)
	if all := calls(); len(all) != 1 || flagOf(all[0], "--session-id") != rec.Session || !strings.Contains(c.ZoomAgent(name), "You are subagent [Again], the user's own") {
		t.Fatalf("its calls: %q", all)
	}
}
