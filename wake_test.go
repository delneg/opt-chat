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
	t.Parallel()
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
	t.Parallel()
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
	if _, err := w.add(time.Now().Add(150*time.Millisecond), "soon", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := w.add(time.Now().Add(time.Hour), "later", 0); err != nil {
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
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	if n := len(a.mcpList()); n != 5 {
		t.Fatalf("tools when off: %d", n)
	}
	if got, _ := callTool(a, false, "", "wake", toolArgs{At: "1h", Message: "m"}); got != "Wake-ups are off: opt-chat runs with -wake=false." {
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
	call := func(fromAgent bool, x toolArgs) string { s, _ := callTool(a, fromAgent, "", "wake", x); return s }
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
	a.woken = false
	if got := call(false, toolArgs{At: "09:00", Every: "1d", Message: "standup"}); !strings.HasPrefix(got, "Wake-up 2 set for ") || !strings.HasSuffix(got, " 09:00, every 24h.") {
		t.Fatalf("set every: %q", got)
	}
	if got := call(false, toolArgs{Every: "2h", Message: "poll"}); !strings.HasPrefix(got, "Wake-up 3 set for ") || !strings.HasSuffix(got, ", every 2h.") {
		t.Fatalf("every without at: %q", got)
	}
	if got := call(false, toolArgs{At: "1h", Every: "5m", Message: "m"}); got != "every must be a duration of 10m or more (2h, 24h, 7d)." {
		t.Fatalf("too often: %q", got)
	}
	if got := call(false, toolArgs{}); !strings.Contains(got, ", every 24h (set ") || strings.Count(got, "\n") != 2 {
		t.Fatalf("list with every: %q", got)
	}
	if got := call(false, toolArgs{Cancel: 1}); got != "Wake-up 1 canceled." {
		t.Fatalf("cancel: %q", got)
	}
	if got := call(false, toolArgs{Cancel: 1}); got != "No wake-up 1 pending." {
		t.Fatalf("cancel again: %q", got)
	}
}

func TestWakeOnly(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestParseEvery(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]time.Duration{"24h": 24 * time.Hour, " 7d ": 7 * 24 * time.Hour, "90m": 90 * time.Minute, "10m": 10 * time.Minute} {
		if got, err := parseEvery(in); err != nil || got != want {
			t.Fatalf("parseEvery(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"soon", "", "-1h", "0d", "1.5d"} {
		if _, err := parseEvery(in); err == nil {
			t.Fatalf("parseEvery(%q): no error", in)
		}
	}
	for d, want := range map[time.Duration]string{24 * time.Hour: "24h", 7 * 24 * time.Hour: "7d", 90 * time.Minute: "1h30m", 10 * time.Minute: "10m", 25 * time.Hour: "25h"} {
		if got := everyText(d); got != want {
			t.Fatalf("everyText(%v) = %q, want %q", d, got, want)
		}
	}
}

// A repeating wake-up's next time is past now, and one of whole days keeps
// its time of day across a change of the clocks.
func TestNextWake(t *testing.T) {
	t.Parallel()
	ams, err := time.LoadLocation("Europe/Amsterdam") // clocks went back on 2026-10-25
	if err != nil {
		t.Skip(err)
	}
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, ams)
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, ams)
	if got, want := nextWake(at, 24*time.Hour, now), time.Date(2026, 10, 27, 9, 0, 0, 0, ams); !got.Equal(want) {
		t.Fatalf("daily: %v, want %v", got, want)
	}
	if got, want := nextWake(at, 7*24*time.Hour, now), time.Date(2026, 10, 31, 9, 0, 0, 0, ams); !got.Equal(want) {
		t.Fatalf("weekly: %v, want %v", got, want)
	}
	if got, want := nextWake(at, 25*time.Hour, now), at.Add(75*time.Hour); !got.Equal(want) {
		t.Fatalf("every 25h: %v, want %v", got, want)
	}
}

// A repeating wake-up stays pending at its next time each time it fires,
// says how often it comes and its id, outlasts a restart, and ends when
// canceled; one missed while opt-chat was down comes once.
func TestWakerEvery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	set := time.Now().Add(-50 * time.Hour).Truncate(time.Second)
	b, _ := json.Marshal(wakeUp{ID: 4, At: set.Add(time.Hour), Set: set, Message: "standup notes", Every: "24h"})
	os.WriteFile(filepath.Join(dir, "wake.jsonl"), append(b, '\n'), 0o600)
	var f fired
	w, err := startWaker(dir, f.add)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the missed wake-up", func() bool { return len(f.all()) == 1 })
	if got, want := f.all()[0], "[opt-chat] wake-up you set at "+set.Local().Format("2006-01-02 15:04")+", every 24h (id 4): standup notes"; got != want {
		t.Fatalf("missed: %q, want %q", got, want)
	}
	if p := w.pending(); len(p) != 1 || !p[0].At.Equal(set.Add(73*time.Hour)) || p[0].Every != "24h" {
		t.Fatalf("pending after: %+v", p)
	}
	u, err := w.add(time.Now().Add(100*time.Millisecond), "tick", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two ticks", func() bool { return len(f.all()) == 3 })
	w.stop()
	time.Sleep(200 * time.Millisecond)
	if n := len(f.all()); n != 3 {
		t.Fatalf("fired %d after stop", n)
	}
	w2, err := startWaker(dir, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w2.stop()
	if p := w2.pending(); len(p) != 2 {
		t.Fatalf("after a restart: %+v", p)
	}
	if !w2.cancel(u.ID) || len(w2.pending()) != 1 {
		t.Fatal("cancel")
	}
}
