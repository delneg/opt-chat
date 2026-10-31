package main

import (
	"encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	h := newHarness(t, c)
	a := h.agent
	armResume(a)

	a.Spawn("Worker", "agent-hit-limit", "", "", 0, nil)
	waitFor(t, "Worker's limit report", func() bool { return workLine(c, "[Worker] failed: Claude AI usage limit reached") })
	h.input(message{kind: "user", text: "turn-hit-limit"})
	waitFor(t, "the turn's No reply", func() bool { return workLine(c, "[opt-chat] No reply: Claude AI usage limit reached") })

	deadline := time.Now().Add(8 * time.Second)
	for !workLine(c, "[opt-chat] usage limit reset") || !strings.Contains(agentPlain(c.dir, "Worker"), "usage limit") {
		if time.Now().After(deadline) {
			t.Fatalf("nothing resumed at the reset: turn %v, Worker %q", workLine(c, "[opt-chat] usage limit reset"), agentPlain(c.dir, "Worker"))
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
	t.Parallel()
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
		{"You've hit your weekly limit · resets Mon 12:00am (America/New_York)", time.Date(2026, 10, 26, 0, 0, 0, 0, ny)},
		{"You've hit your weekly limit · resets Fri 9am (America/New_York)", time.Date(2026, 10, 23, 9, 0, 0, 0, ny)},
		{"You've hit your weekly limit · resets Friday 7am (America/New_York)", time.Date(2026, 10, 30, 7, 0, 0, 0, ny)},
		{"You've hit your monthly spend limit · raise it at https://claude.ai/settings/usage?from=cc_cli_limit_message · your session limit resets 3:45pm (America/New_York)", time.Date(2026, 10, 23, 15, 45, 0, 0, ny)},
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
	t.Parallel()
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
		"Usage limit reached · continuing automatically at 3:45pm",
		"You've hit your monthly spend limit · raise it at https://claude.ai/settings/usage?from=cc_cli_limit_message · your session limit resets 3:45pm",
		"You've hit your individual spend limit · ask your admin for a higher limit · your weekly limit resets Mon 12:00am",
		"You've hit your org's monthly spend limit · visit https://claude.ai/admin-settings/usage to raise it · your Opus limit resets 3:45pm",
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
		"You've hit your monthly spend limit · raise it at https://claude.ai/settings/usage?from=cc_cli_limit_message",
		"You've hit your individual usage limit · ask your admin for a higher limit",
		"You've hit your channel's monthly spend limit · an org owner or channel manager can raise it in the channel's Claude settings",
		"You've used 85% of your session limit · resets 3:45pm",
		"API Error: Usage credits required for 1M context · run /usage-credits to turn them on",
		"Fable 5.1 now uses usage credits · the prompt to confirm went unanswered — nothing was sent · answer it where this session is running, or /model to change",
		"Credit balance is too low",
		"API Error: Server is temporarily limiting requests (not your usage limit)",
		"Request too large for the API's 32MB request limit",
		"upstream rate limit exceeded",
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

// Claude Code's rate_limit_event says when the plan refused a request and
// until when: a failure while it stands is the limit whatever its words, and
// resumes at the event's reset. Usage credits that still pay don't stop it,
// nor does one only bought credits lift, and a later event that allows
// requests clears it.
func TestRejectedLimitEvent(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := newHarness(t, c).agent
	armResume(a)
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Minute)
	rejected := func(overage string) limitInfo {
		return limitInfo{Status: "rejected", ResetsAt: reset.Unix(), RateLimitType: "seven_day", OverageStatus: overage}
	}
	const words = "API Error: request refused"

	a.noteLimits(rejected("allowed"))
	if line := a.limitHit(words, "Paid"); line != "" {
		t.Errorf("credits still paying, limitHit = %q, want none", line)
	}
	a.noteLimits(limitInfo{Status: "rejected", ResetsAt: reset.Unix(), ErrorCode: "credits_required"})
	if line := a.limitHit(words, "Credits"); line != "" {
		t.Errorf("credits required, limitHit = %q, want none: no reset lifts it", line)
	}
	a.noteLimits(rejected("rejected"))
	want := reset.Add(a.resetGrace).Local().Format("Jan 2 15:04")
	if line := a.limitHit(words, "Worker"); !strings.Contains(line, want) {
		t.Errorf("rejected event, limitHit = %q, want the reset %s", line, want)
	}
	a.noteLimits(limitInfo{Status: "allowed"})
	if line := a.limitHit(words, "Later"); line != "" {
		t.Errorf("after an allowed event, limitHit = %q, want none", line)
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
	t.Parallel()
	dir := t.TempDir()
	var f fired
	got := func(s resumeState) { f.add(strings.Join(stoppedBy(s), ",")) }
	r, err := startResumer(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	at, ok := r.note("A", now.Add(time.Hour), 90*time.Second, now)
	if !ok || !at.Equal(now.Add(time.Hour+90*time.Second)) {
		t.Fatalf("at %v %v", at, ok)
	}
	if at2, _ := r.note("A", now.Add(30*time.Minute), 90*time.Second, now); !at2.Equal(at) {
		t.Fatalf("an earlier reset moved it back: %v", at2)
	}
	r.note("", now.Add(2*time.Hour), 0, now)
	r.stop()

	r2, err := startResumer(dir, got)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.stop()
	if len(r2.st.Due) != 2 || !r2.st.Due["A"].Equal(at) || !r2.st.Due[""].Equal(now.Add(2*time.Hour)) {
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
	b, _ := json.Marshal(resumeState{Due: map[string]time.Time{"C": now.Add(-time.Minute)}})
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
	r4.went("C")
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

// A rate_limit_event that refuses one model's weekly window speaks for that
// model's calls only: a subagent on another model that fails for another
// reason is not waiting on it. Found by a TLA+ model of the resume (property NoWaitOnNonLimit).
func TestModelLimitEventIsNotEveryones(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := newHarness(t, c).agent
	armResume(a)
	reset := time.Now().Add(72 * time.Hour).Unix()
	for _, window := range []string{"seven_day_opus", "seven_day_sonnet"} {
		a.noteLimits(limitInfo{Status: "rejected", ResetsAt: reset, RateLimitType: window})
		if line := a.limitHit("API Error: 500 Internal server error", "Worker"); line != "" {
			t.Errorf("%s rejected, another model's failure: limitHit = %q, want none", window, line)
		}
	}
}

// A subagent the limit stopped that was set to work again and finished
// before the reset is not told at the reset to pick up where it stopped.
// Found by a TLA+ model of the resume (property NoResumeOfFinished).
func TestResumeSkipsSubagentThatFinished(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := newHarness(t, c).agent
	armResume(a)

	a.Spawn("Worker", "agent-hit-limit", "", "", 0, nil)
	waitFor(t, "Worker's limit report", func() bool { return workLine(c, "[Worker] failed: Claude AI usage limit reached") })
	if out, ok := a.tell("Worker", tell{text: "check the branch"}); !ok {
		t.Fatalf("tell: %s", out)
	}
	waitFor(t, "Worker's second report", func() bool {
		n := 0
		for _, l := range logged(c) {
			if strings.Contains(l, "[Worker]") {
				n++
			}
		}
		return n >= 2
	})
	time.Sleep(3500 * time.Millisecond) // past the reset the limit named
	if strings.Contains(agentPlain(c.dir, "Worker"), "usage limit has reset") {
		t.Errorf("Worker finished before the reset, then was told to pick up after it:\n%s", agentPlain(c.dir, "Worker"))
	}
}

// Each one the limit stopped goes on at its own reset: a subagent stopped
// by the five-hour window is not held until Opt's weekly Opus reset. Found
// by a TLA+ model of the resume (property NoLateResume).
func TestResumeEachAtItsOwnReset(t *testing.T) {
	t.Parallel()
	var f fired
	r, err := startResumer(t.TempDir(), func(s resumeState) { f.add(strings.Join(stoppedBy(s), ",")) })
	if err != nil {
		t.Fatal(err)
	}
	defer r.stop()
	now := time.Now()
	r.note("", now.Add(72*time.Hour), 0, now)
	r.note("A", now.Add(100*time.Millisecond), 0, now)
	waitFor(t, "A's resume", func() bool { return len(f.all()) == 1 })
	if got := f.all()[0]; got != "A" {
		t.Errorf("resumed %q at A's reset, want A alone", got)
	}
}

// A reset time that has just passed as the message is read is that time,
// not the same time a day or a week on.
func TestLimitResetJustPassed(t *testing.T) {
	t.Parallel()
	loc := mustLoad(t, "Asia/Manila")
	now := time.Date(2026, 10, 26, 4, 0, 20, 0, loc) // a Monday
	for _, text := range []string{
		"You've hit your session limit · resets 4am (Asia/Manila)",
		"You've hit your weekly limit · resets Mon 4:00am (Asia/Manila)",
	} {
		at, ok := limitReset(text, now)
		if !ok || at.After(now) || now.Sub(at) > time.Minute {
			t.Errorf("%q read at %v: %v %v, want 4:00 the same day", text, now, at, ok)
		}
	}
}

// stoppedBy names who a resume goes out for, "" for Opt's turn.
func stoppedBy(s resumeState) []string { return slices.Sorted(maps.Keys(s.Due)) }
