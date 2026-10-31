package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loopHookFor is a Stop hook for the tests, run in a folder of its own: it
// counts its calls in n ($n is this one's), keeps each input in in, one a
// line, and then runs body.
func loopHookFor(t *testing.T, body string) *loopHook {
	t.Helper()
	dir := t.TempDir()
	f := filepath.Join(dir, "hook.sh")
	script := "#!/bin/sh\nn=$(( $(cat n 2>/dev/null || echo 0) + 1 )); echo $n > n\ncat >> in; echo >> in\n" + body + "\n"
	if err := os.WriteFile(f, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return &loopHook{Command: f, Dir: dir}
}

func hookInputs(t *testing.T, l *loopHook) []map[string]any {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(l.Dir, "in"))
	var out []map[string]any
	for line := range strings.Lines(string(b)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("hook input %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// loopReports catches a's reports: report waits for the next; quiet
// fails on one within a moment.
func loopReports(t *testing.T, a *Agent) (report func() string, quiet func()) {
	reported := make(chan string, 8)
	a.report = func(text string) { a.chat.Log("work", text); reported <- text }
	report = func() string {
		t.Helper()
		select {
		case got := <-reported:
			return got
		case <-time.After(15 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	quiet = func() {
		t.Helper()
		select {
		case got := <-reported:
			t.Fatalf("another report: %q", got)
		case <-time.After(400 * time.Millisecond):
		}
	}
	return report, quiet
}

// A loop's hook keeps a subagent at work, turn after turn, in one call,
// with nothing in the chat until its loop ends: then one report, the
// answer to the message that asks for it.
func TestAgentLoop(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	report, quiet := loopReports(t, a)
	l := loopHookFor(t, `[ $n -le 3 ] && echo '{"decision":"block","reason":"go on '$n'"}'; exit 0`)
	if got := a.Spawn("Loop", "loop-task", "", "", 0, l); !strings.HasPrefix(got, "Started [Loop]") {
		t.Fatal(got)
	}
	got := report()
	if !strings.HasPrefix(got, fmt.Sprintf("[Loop] answer to %q", loopFeedback+"go on 3")) || !strings.Contains(got, "loop stopped: its Stop hook let it stop") {
		t.Fatalf("report: %q", got)
	}
	quiet()
	if n := len(calls()); n != 1 {
		t.Fatalf("%d calls, want 1", n)
	}
	in := hookInputs(t, l)
	if len(in) != 4 {
		t.Fatalf("%d hook calls, want 4", len(in))
	}
	rec, _ := a.loadRecord("Loop")
	for i, m := range in {
		if m["session_id"] != rec.Session || m["cwd"] != l.Dir || m["hook_event_name"] != "Stop" || m["stop_hook_active"] != (i > 0) ||
			!strings.HasSuffix(m["transcript_path"].(string), rec.Session+".jsonl") {
			t.Fatalf("hook input %d: %v", i, m)
		}
	}
	if last := in[1]["last_assistant_message"].(string); !strings.HasPrefix(last, fmt.Sprintf("answer to %q", loopFeedback+"go on 1")) {
		t.Fatalf("last message: %q", last)
	}
	if rec.Loop == nil || rec.Loop.Command != l.Command || rec.Loop.Dir != l.Dir {
		t.Fatalf("record: %+v", rec.Loop)
	}
	log := agentPlain(c.dir, "Loop")
	for _, s := range []string{"loop: go on 1", "loop: go on 2", "loop: go on 3"} {
		if !strings.Contains(log, s) {
			t.Fatalf("no %q in its log: %q", s, log)
		}
	}
	for _, m := range logged(c) {
		if strings.Contains(m, "go on 1") || strings.Contains(m, "go on 2") {
			t.Fatalf("the loop reached the chat: %q", m)
		}
	}
}

// The loop ends, with a report saying why, at its cap, when its hook fails,
// and when its hook takes too long; never hangs.
func TestAgentLoopEnds(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	report, _ := loopReports(t, a)

	l := loopHookFor(t, `echo '{"decision":"block","reason":"again"}'`)
	l.Max = 2
	a.Spawn("Capped", "loop-task", "", "", 0, l)
	if got := report(); !strings.Contains(got, "loop stopped: 2 turns, its cap") {
		t.Fatalf("capped: %q", got)
	}
	if n := len(hookInputs(t, l)); n != 2 { // at the cap, it isn't asked
		t.Fatalf("capped: %d hook calls, want 2", n)
	}

	l = loopHookFor(t, `echo boom >&2; exit 1`)
	a.Spawn("Broken", "loop-task", "", "", 0, l)
	if got := report(); !strings.Contains(got, "loop stopped: its Stop hook failed") || !strings.Contains(got, "boom") {
		t.Fatalf("broken: %q", got)
	}

	l = loopHookFor(t, `sleep 5`)
	l.timeout = 300 * time.Millisecond
	start := time.Now()
	a.Spawn("Slow", "loop-task", "", "", 0, l)
	if got := report(); !strings.Contains(got, "loop stopped: its Stop hook failed: timed out") {
		t.Fatalf("slow: %q", got)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Fatalf("slow hook took %v", d)
	}

	// As Claude Code: exit 2 blocks, with stderr as the reason.
	l = loopHookFor(t, `[ $n -eq 1 ] && { echo 'from stderr' >&2; exit 2; }; exit 0`)
	a.Spawn("Two", "loop-task", "", "", 0, l)
	if got := report(); !strings.Contains(got, "loop stopped: its Stop hook let it stop") || !strings.Contains(agentPlain(c.dir, "Two"), "loop: from stderr") {
		t.Fatalf("exit 2: %q", got)
	}

	// A bad loop is refused.
	for _, bad := range []*loopHook{{Command: " ", Dir: l.Dir}, {Command: "x", Dir: "rel"}, {Command: "x", Dir: "/no/such/dir"}, {Command: "x", Dir: l.Dir, Max: -1}} {
		if got := a.Spawn("Bad", "x", "", "", 0, bad); !strings.HasPrefix(got, "loop") {
			t.Fatalf("%+v: %q", bad, got)
		}
	}
}

// A tell to a subagent in a loop waits for its turn's end, and goes ahead
// of the loop's next step; Stop on the page ends the loop.
func TestAgentLoopTellAndStop(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	report, _ := loopReports(t, a)

	l := loopHookFor(t, `[ $n -eq 1 ] && { sleep 1; echo '{"decision":"block","reason":"go on"}'; }; exit 0`)
	a.Spawn("Told", "loop-task", "", "", 0, l)
	waitFor(t, "its hook", func() bool { _, err := os.Stat(filepath.Join(l.Dir, "n")); return err == nil })
	if got := a.Tell("Told", "held-tell"); !strings.HasPrefix(got, "Sent: [Told] is in a loop") {
		t.Fatal(got)
	}
	if got := report(); !strings.HasPrefix(got, `[Told] answer to "held-tell"`) {
		t.Fatalf("told: %q", got)
	}
	if strings.Contains(agentPlain(c.dir, "Told"), "loop: go on") {
		t.Fatal("the hook's step went ahead of the tell")
	}

	// One that comes as the loop ends is not lost: it goes after the report.
	l = loopHookFor(t, `[ $n -eq 1 ] && sleep 1; exit 0`)
	a.Spawn("Late", "loop-task", "", "", 0, l)
	waitFor(t, "its hook", func() bool { _, err := os.Stat(filepath.Join(l.Dir, "n")); return err == nil })
	a.Tell("Late", "late-tell")
	if got := report(); !strings.Contains(got, "loop-task") {
		t.Fatalf("late, first: %q", got)
	}
	if got := report(); !strings.HasPrefix(got, `[Late] answer to "late-tell"`) {
		t.Fatalf("late, second: %q", got)
	}

	l = loopHookFor(t, `[ $n -eq 1 ] && echo '{"decision":"block","reason":"wait-for-stop"}'; exit 0`)
	a.Spawn("Stopped", "loop-task", "", "", 0, l)
	waitFor(t, "its loop", func() bool { return strings.Contains(agentPlain(c.dir, "Stopped"), "loop: wait-for-stop") })
	time.Sleep(200 * time.Millisecond)
	if !a.StopAgent("Stopped") {
		t.Fatal("no stop")
	}
	waitFor(t, "the stop logged", func() bool {
		return strings.Contains(strings.Join(logged(c), "\n"), "[Stopped] stopped: the user stopped it")
	})
	time.Sleep(300 * time.Millisecond)
	if n := len(hookInputs(t, l)); n != 1 {
		t.Fatalf("%d hook calls after the stop", n)
	}
}

// Past its window, a subagent in a loop is compacted between turns, and
// its next step comes with the view and its task again.
func TestAgentLoopCompacts(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	report, _ := loopReports(t, a)
	l := loopHookFor(t, `[ $n -eq 1 ] && echo '{"decision":"block","reason":"next step"}'; exit 0`)
	a.Spawn("Big", "big-task", "", "", 250_000, l)
	got := report()
	log := agentPlain(c.dir, "Big")
	if !strings.Contains(got, "loop stopped") || !strings.Contains(log, "compacted: 300000 tokens to 20000") ||
		!strings.Contains(log, "anchored: view true, task true, message true") {
		t.Fatalf("report %q, log %q", got, log)
	}
}

// Sent now to a subagent in a loop, the user's message cuts its turn and
// goes first, with a tell held for the turn's end, ahead of the hook, which
// then sees a turn of the user's. Both are in its log at once.
func TestAgentLoopTellNow(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	report, _ := loopReports(t, a)
	l := loopHookFor(t, `[ $n -le 1 ] && echo '{"decision":"block","reason":"go on '$n'"}'; exit 0`)
	a.Spawn("Loop", "slow-task", "", "", 0, l)
	waitFor(t, "its long call", func() bool { return strings.Contains(agentPlain(c.dir, "Loop"), "sleep 600") })
	if _, ok := a.TellUser("Loop", "first", nil, false); !ok || !strings.Contains(agentPlain(c.dir, "Loop"), "user: first") {
		t.Fatalf("a tell held for its turn's end is not in its log: %q", agentPlain(c.dir, "Loop"))
	}
	if out, ok := a.TellUser("Loop", "hurry", nil, true); !ok || out != "Sent: [Loop] was interrupted to read it now." {
		t.Fatal(out, ok)
	}
	if got := report(); !strings.Contains(got, fmt.Sprintf("answer to %q", loopFeedback+"go on 1")) {
		t.Fatalf("report: %q", got)
	}
	log := agentPlain(c.dir, "Loop")
	if told, hurried := strings.Index(log, "user: hurry"), strings.Index(log, "agent: hurried: "); told < 0 || hurried < told || strings.Count(log, "user: hurry") != 1 {
		t.Fatalf("its log: %q", log)
	}
	if n := strings.Count(strings.Join(fakeFiles(a, ".ctl"), ""), `"interrupt"`); n != 1 {
		t.Fatalf("%d interrupts", n)
	}
	if in := hookInputs(t, l); len(in) != 2 || in[0]["stop_hook_active"] != false || !strings.HasPrefix(in[0]["last_assistant_message"].(string), "hurried: ") {
		t.Fatalf("hook inputs: %v", in)
	}
}
