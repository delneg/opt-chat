package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 15, 20, 0, 0, 0, time.Local)
	for in, want := range map[string]time.Time{
		"90m":                       now.Add(90 * time.Minute),
		" 2h ":                      now.Add(2 * time.Hour),
		"21:30":                     time.Date(2026, 10, 15, 21, 30, 0, 0, time.Local),
		"09:00":                     time.Date(2026, 10, 16, 9, 0, 0, 0, time.Local), // passed today: tomorrow
		"2026-10-20 09:00":          time.Date(2026, 10, 20, 9, 0, 0, 0, time.Local),
		"2026-10-20T09:00:00+03:00": time.Date(2026, 10, 20, 9, 0, 0, 0, time.FixedZone("", 3*3600)),
	} {
		if got, err := parseWhen(in, now); err != nil || !got.Equal(want) {
			t.Fatalf("parseWhen(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "soon", "-5m", "2026-10-01 09:00"} {
		if _, err := parseWhen(in, now); err == nil {
			t.Fatalf("parseWhen(%q): no error", in)
		}
	}
}

// fired collects what a waker delivers.
type fired struct {
	mu   sync.Mutex
	msgs []string
}

func (f *fired) add(s string) { f.mu.Lock(); f.msgs = append(f.msgs, s); f.mu.Unlock() }
func (f *fired) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.msgs...)
}

// Wake-ups are kept in wake.jsonl: one missed while opt-chat was down comes
// once as it starts, the rest when due, and pending ones survive a restart.
func TestWaker(t *testing.T) {
	dir := t.TempDir()
	set := time.Date(2026, 10, 1, 9, 5, 0, 0, time.Local)
	b, _ := json.Marshal(wakeUp{ID: 3, At: set.Add(time.Hour), Set: set, Message: "check the deploy"})
	os.WriteFile(filepath.Join(dir, "wake.jsonl"), append(b, '\n'), 0o600)
	var f fired
	w, err := startWaker(dir, f.add)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the missed wake-up", func() bool { return len(f.all()) == 1 })
	if got := f.all()[0]; got != "[opt-chat] wake-up you set at 2026-10-01 09:05: check the deploy" {
		t.Fatalf("missed: %q", got)
	}
	if _, err := w.add(time.Now().Add(150*time.Millisecond), "soon"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.add(time.Now().Add(time.Hour), "later"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the due wake-up", func() bool { return len(f.all()) == 2 })
	if got := f.all()[1]; !strings.HasPrefix(got, "[opt-chat] wake-up you set at ") || !strings.HasSuffix(got, ": soon") {
		t.Fatalf("due: %q", got)
	}
	w.stop()
	var g fired
	w2, err := startWaker(dir, g.add)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.stop()
	if p := w2.pending(); len(p) != 1 || p[0].Message != "later" {
		t.Fatalf("after a restart: %+v", p)
	}
	time.Sleep(50 * time.Millisecond)
	if len(g.all()) != 0 || len(f.all()) != 2 {
		t.Fatalf("fired again: %q %q", f.all(), g.all())
	}
	if !w2.cancel(w2.pending()[0].ID) || len(w2.pending()) != 0 || w2.cancel(99) {
		t.Fatal("cancel")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "wake.jsonl")); len(b) != 0 {
		t.Fatalf("wake.jsonl after cancel: %q", b)
	}
}

// The wake tool: set, list and cancel; never from a subagent, and a turn a
// wake-up started sets none; listed only when on.
func TestWakeTool(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	if n := len(a.mcpList()); n != 5 {
		t.Fatalf("tools when off: %d", n)
	}
	if got, _ := callTool(a, false, "wake", toolArgs{At: "1h", Message: "m"}); got != "Wake-ups are off: opt-chat runs with -wake=false." {
		t.Fatalf("off: %q", got)
	}
	w, err := startWaker(c.dir, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w.stop()
	a.wakes = w
	if n := len(a.mcpList()); n != 6 {
		t.Fatalf("tools when on: %d", n)
	}
	call := func(fromAgent bool, x toolArgs) string { s, _ := callTool(a, fromAgent, "wake", x); return s }
	if got := call(false, toolArgs{}); got != "No wake-ups pending." {
		t.Fatalf("empty list: %q", got)
	}
	if got := call(false, toolArgs{At: "2h", Message: " look at the build "}); !strings.HasPrefix(got, "Wake-up 1 set for ") {
		t.Fatalf("set: %q", got)
	}
	if got := call(false, toolArgs{At: "soon", Message: "x"}); !strings.Contains(got, "at must be") {
		t.Fatalf("bad at: %q", got)
	}
	if got := call(false, toolArgs{At: "1h"}); got != "No message given." {
		t.Fatalf("no message: %q", got)
	}
	if got := call(false, toolArgs{}); !strings.HasPrefix(got, "1 ") || !strings.HasSuffix(got, ": look at the build") {
		t.Fatalf("list: %q", got)
	}
	if got := call(true, toolArgs{At: "1h", Message: "m"}); got != "Only Opt sets wake-ups." {
		t.Fatalf("a subagent's: %q", got)
	}
	a.woken = true
	if got := call(false, toolArgs{At: "1h", Message: "m"}); got != "A turn a wake-up started sets no new wake-ups." {
		t.Fatalf("woken: %q", got)
	}
	if got := call(false, toolArgs{Cancel: 1}); got != "Wake-up 1 canceled." {
		t.Fatalf("cancel: %q", got)
	}
	if got := call(false, toolArgs{Cancel: 1}); got != "No wake-up 1 pending." {
		t.Fatalf("cancel again: %q", got)
	}
}

func TestWakeOnly(t *testing.T) {
	wake := message{kind: "work", text: wakeFrame + "2026-10-15 20:00: x"}
	for _, tc := range []struct {
		msgs []message
		want bool
	}{
		{[]message{wake}, true},
		{[]message{wake, {kind: "work", text: "[A] done"}}, true},
		{[]message{wake, {kind: "user", text: "hi"}}, false},
		{[]message{{kind: "work", text: "[A] done"}}, false},
		{[]message{{kind: "user", text: wakeFrame}}, false},
	} {
		if got := wakeOnly(tc.msgs); got != tc.want {
			t.Fatalf("wakeOnly(%v) = %v", tc.msgs, got)
		}
	}
}

// The wake tool runs unasked, as the chat's other tools do.
func TestWakeAllowed(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	a, calls := fakeAgent(t, c)
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got := flagOf(calls()[0], "--allowedTools"); !strings.Contains(got, "mcp__opt__wake") {
		t.Fatalf("allowed: %s", got)
	}
}
