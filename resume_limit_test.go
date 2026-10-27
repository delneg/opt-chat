package main

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// armResume turns on resume-at-reset for a test agent.
func armResume(a *Agent) {
	r, err := startResumer(a.chat.dir, a.resumeAfterLimit)
	if err != nil {
		panic(err)
	}
	a.mu.Lock()
	a.resumes = r
	a.mu.Unlock()
	go func() { <-a.life.Done(); r.stop() }()
}

// A turn or a subagent stopped by the plan's usage limit goes on at its
// reset: one turn starts, and the subagent is resumed from its session.
func TestResumeAtReset(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	h := newHarness(t, c)
	a := h.agent
	armResume(a)

	a.Spawn("Worker", "agent-hit-limit", "", "", 0, nil)
	waitFor(t, "Worker's limit report", func() bool { return workLine(c, "[Worker] failed: Claude AI usage limit reached") })
	h.input(message{kind: "user", text: "turn-hit-limit"})
	waitFor(t, "the turn's No reply", func() bool { return workLine(c, "[opt-chat] No reply: Claude AI usage limit reached") })

	deadline := time.Now().Add(8 * time.Second)
	for !workLine(c, "[opt-chat] usage limit reset") || !strings.Contains(c.ZoomAgent("Worker"), "usage limit") {
		if time.Now().After(deadline) {
			t.Fatalf("nothing resumed at the reset: turn %v, Worker %q", workLine(c, "[opt-chat] usage limit reset"), c.ZoomAgent("Worker"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func workLine(c *Chat, prefix string) bool {
	for _, l := range logged(c) {
		if strings.Contains(l, prefix) {
			return true
		}
	}
	return false
}

func TestLimitReset(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 10, 23, 7, 36, 0, 0, ny)
	for _, tc := range []struct {
		in   string
		want time.Time
	}{
		{"You've hit your session limit · resets 4am (America/New_York)", time.Date(2026, 10, 24, 4, 0, 0, 0, ny)},
		{"You've hit your session limit · resets 9:30am (America/New_York)", time.Date(2026, 10, 23, 9, 30, 0, 0, ny)},
		{"You've hit your weekly limit · resets Oct 26, 11am (America/New_York)", time.Date(2026, 10, 26, 11, 0, 0, 0, ny)},
		{"You've hit your limit · resets 12am (UTC)", time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)},
		{"Your limit will reset at 3pm (America/New_York)", time.Date(2026, 10, 23, 15, 0, 0, 0, mustLoad(t, "America/New_York"))},
		{"5-hour limit reached ∙ resets 16:00 (UTC)", time.Date(2026, 10, 23, 16, 0, 0, 0, time.UTC)},
		{"usage limit reached, resets in 2 hours", now.Add(2 * time.Hour)},
		{"Claude AI usage limit reached|1792720194", time.Unix(1792720194, 0)},
		{"Claude AI usage limit reached|1792720194123", time.Unix(1792720194, 0)},
	} {
		got, ok := limitReset(tc.in, now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("limitReset(%q) = %v, %v; want %v", tc.in, got, ok, tc.want)
		}
		if !usageLimit(tc.in) {
			t.Errorf("usageLimit(%q) = false", tc.in)
		}
	}
	for _, in := range []string{"overloaded", "API Error: 529", "resets 4 (UTC)", ""} {
		if _, ok := limitReset(in, now); ok {
			t.Errorf("limitReset(%q) read a time", in)
		}
	}
}

// Only the plan's usage limit resumes at a reset. Other limits carry no
// reset time, so taking one for it would wait for the five-hour reset and
// resume a call that fails the same way again.
func TestUsageLimitWording(t *testing.T) {
	for _, in := range []string{
		"You've hit your session limit · resets 4am (Asia/Manila)",
		"You've hit your weekly limit · resets Oct 26, 11am (UTC)",
		"You've hit your Opus limit · resets Oct 26, 11am (UTC)",
		"You've hit your Sonnet limit · resets Oct 26, 11am (UTC)",
		"You've reached your Fable limit.",
		"You've hit your limit · resets 12am (UTC)",
		"Usage limit reached · resets 3pm",
		"Claude AI usage limit reached|1792720194",
		"API Error: usage limit reached",
		"Claude usage limit reached. Your limit will reset at 5pm (UTC).",
		"5-hour limit reached ∙ resets 16:00 (UTC)",
		"Opus weekly limit reached ∙ resets Oct 26, 11am",
		"Lower-priority mode ended · you have reached your weekly usage limit",
	} {
		if !usageLimit(in) {
			t.Errorf("usageLimit(%q) = false, want true", in)
		}
	}
	for _, in := range []string{
		"Context limit reached · /compact or /clear to continue",
		`API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"Rate limit reached for requests"}}`,
		"Error: max turns limit reached",
		"Output token limit reached",
		"Server is temporarily limiting requests (not your usage limit) · Rate limited",
		"You've hit your monthly spend limit.",
		"You've hit your channel's monthly spend limit.",
		"You've hit your monthly limit, raise it below, or it resets next month.",
		"You've hit your team's shared budget. /model to switch models.",
		"You've hit your usage credit limit",
		"Your group's usage limit is set to $0",
		"Your org's monthly usage limit reached",
		"You're out of usage credits. Run /usage-credits to keep using it",
		"Your org is out of usage · contact your admin",
		"This request would exceed your account's rate limit",
		"spend limit reached (daily; resets 2026-08-08 00:00 UTC)",
		"overloaded",
		"Request rejected (429)",
		"Prompt is too long",
		"",
	} {
		if usageLimit(in) {
			t.Errorf("usageLimit(%q) = true, want false", in)
		}
	}
}

func mustLoad(t *testing.T, name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// A pending resume outlasts a restart; a hit that comes again moves the
// time, never sooner; a stale time backs off; a string of hits stops.
func TestResumer(t *testing.T) {
	dir := t.TempDir()
	var f fired
	got := func(s resumeState) { f.add(strings.Join(s.Agents, ",")) }
	r, err := startResumer(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at, ok := r.note("A", now.Add(time.Hour), 90*time.Second, now)
	if !ok || !at.Equal(now.Add(time.Hour+90*time.Second)) {
		t.Fatalf("at %v %v", at, ok)
	}
	if at2, _ := r.note("", now.Add(30*time.Minute), 90*time.Second, now); !at2.Equal(at) {
		t.Fatalf("an earlier reset moved it back: %v", at2)
	}
	r.stop()

	r2, err := startResumer(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.stop()
	if !r2.st.Turn || len(r2.st.Agents) != 1 || !r2.st.At.Equal(at) {
		t.Fatalf("after a restart: %+v", r2.st)
	}
	if len(f.all()) != 0 {
		t.Fatalf("fired early: %q", f.all())
	}

	// a reset already past: not before the back-off
	r3, _ := startResumer(t.TempDir(), got)
	defer r3.stop()
	at, _ = r3.note("B", now.Add(-time.Hour), 0, now)
	if at.Before(now.Add(5 * time.Minute)) {
		t.Fatalf("stale reset resumes at %v", at)
	}

	// missed while down: out as it starts, once
	d2 := t.TempDir()
	b, _ := json.Marshal(resumeState{At: now.Add(-time.Minute), Agents: []string{"C"}})
	os.WriteFile(filepath.Join(d2, "resume.json"), b, 0o600)
	r4, _ := startResumer(d2, got)
	defer r4.stop()
	waitFor(t, "the missed resume", func() bool { return len(f.all()) == 1 })
	if f.all()[0] != "C" || r4.st.Tries != 1 || r4.st.pending() {
		t.Fatalf("missed: %q %+v", f.all(), r4.st)
	}
	again := now.Add(3 * time.Hour)
	if at, ok := r4.note("C", again, 0, now); !ok || at.Before(again) || r4.st.Tries != 1 {
		t.Fatalf("a hit after a resume: %v %v %+v", at, ok, r4.st)
	}
	r4.st.Agents, r4.st.Turn = nil, false
	r4.went()
	if r4.st.Tries != 0 {
		t.Fatal("tries stay after a good run")
	}

	// the limit hits again and again: gives up
	r5, _ := startResumer(t.TempDir(), func(resumeState) {})
	defer r5.stop()
	r5.st.Tries, r5.st.Fired = limitMax, now
	if _, ok := r5.note("D", now.Add(time.Hour), 0, now); ok {
		t.Fatal("kept resuming")
	}
}
