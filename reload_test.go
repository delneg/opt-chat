package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// savedAgent writes the record of a subagent whose session Claude Code
// has saved, as an earlier run left it.
func savedAgent(t *testing.T, a *Agent, name string, r agentRecord) {
	t.Helper()
	r.Session = newSession()
	dir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-fake")
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, r.Session+".jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(a.chat.dir, "agents"), 0o700)
	if r.Model == "" {
		r.Model, r.Task = "mid", "a task"
	}
	a.note(name, func(x *agentRecord) { *x = r })
}

func reports(t *testing.T, a *Agent) func() string {
	reported := make(chan string, 8)
	a.report = func(text string) { a.chat.Log("work", text); reported <- text }
	return func() string {
		t.Helper()
		select {
		case got := <-reported:
			return got
		case <-time.After(15 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
}

func atWork(t *testing.T, c *Chat, name string) {
	t.Helper()
	waitFor(t, name+" at work", func() bool { return strings.Contains(agentPlain(c.dir, name), "agent: working") })
}

func noneLogged(t *testing.T, c *Chat, s string) {
	t.Helper()
	for _, m := range logged(c) {
		if strings.Contains(m, s) {
			t.Fatalf("logged %q", m)
		}
	}
}

// A subagent a quit cut off mid-task goes on by itself at the next start,
// from its saved session, told to check its last tool call; the quit
// sends the chat no "stopped" report for it.
func TestAutoResumeAfterQuit(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	a.autoResume = true
	a.Spawn("Long", "wait-for-stop", "", "", 0, nil)
	atWork(t, c, "Long")
	a.quit()
	noneLogged(t, c, "stopped")
	session := flagOf(calls()[0], "--session-id")
	if rec, _ := a.loadRecord("Long"); rec.State != "stopped" || !rec.Cut {
		t.Fatalf("its record: %+v", rec)
	}

	b, calls := fakeAgent(t, c) // opt-chat again
	b.autoResume = true
	report := reports(t, b)
	msgs, cut := b.recoverAgents()
	if len(msgs) != 0 || !slices.Equal(cut, []string{"Long"}) {
		t.Fatalf("recovered %v, to resume %v", msgs, cut)
	}
	b.resumeCut(cut)
	if got := report(); !strings.Contains(got, fmt.Sprintf("answer to %q", restartNote)) {
		t.Fatalf("report: %q", got)
	}
	if !strings.Contains(restartNote, "may have been cut off") || !strings.Contains(restartNote, "check its effect before redoing it") {
		t.Fatalf("note: %q", restartNote)
	}
	if args := calls()[0]; flagOf(args, "--resume") != session {
		t.Fatalf("resumed with %q", args)
	}
	if !slices.ContainsFunc(logged(c), func(m string) bool { return strings.HasPrefix(m, "work: [opt-chat] ") && strings.Contains(m, "[Long]") }) {
		t.Fatalf("logged: %q", logged(c))
	}
	if rec, _ := b.loadRecord("Long"); rec.Cut || rec.Resumes != 0 || rec.State != "resting" {
		t.Fatalf("after its report: %+v", rec)
	}
}

// One a crash cut off, its record still "working", goes on too.
func TestAutoResumeAfterCrash(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	a.autoResume = true
	report := reports(t, a)
	savedAgent(t, a, "Crashed", agentRecord{State: "working"})
	msgs, cut := a.recoverAgents()
	if len(msgs) != 0 || !slices.Equal(cut, []string{"Crashed"}) {
		t.Fatalf("recovered %v, to resume %v", msgs, cut)
	}
	if rec, _ := a.loadRecord("Crashed"); rec.State != "stopped" || !rec.Cut {
		t.Fatalf("its record: %+v", rec)
	}
	a.resumeCut(cut)
	if got := report(); !strings.Contains(got, fmt.Sprintf("answer to %q", restartNote)) {
		t.Fatalf("report: %q", got)
	}
	if rec, _ := a.loadRecord("Crashed"); flagOf(calls()[0], "--resume") != rec.Session || rec.Resumes != 0 || rec.Cut {
		t.Fatalf("calls %q, record %+v", calls(), rec)
	}
}

// Only those cut off go on: never one the user stopped, one that failed,
// ended or rested, one cut off over a day ago, or one resumed resumeMax
// times without a result since. Those cut off that don't go on are told
// of once.
func TestNoAutoResume(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	a.autoResume = true
	for name, state := range map[string]string{"Halted": "halted", "Failed": "failed", "Ended": "ended", "Resting": "resting", "Legacy": "stopped"} {
		savedAgent(t, a, name, agentRecord{State: state})
	}
	savedAgent(t, a, "Old", agentRecord{State: "stopped", Cut: true})
	day := time.Now().Add(-25 * time.Hour)
	os.Chtimes(a.recordPath("Old"), day, day)
	savedAgent(t, a, "Spent", agentRecord{State: "working", Resumes: resumeMax})
	msgs, cut := a.recoverAgents()
	if len(cut) != 0 {
		t.Fatalf("to resume: %v", cut)
	}
	var texts []string
	for _, m := range msgs {
		texts = append(texts, m.text)
	}
	slices.Sort(texts)
	if len(texts) != 2 || !strings.HasPrefix(texts[0], "[Old] stopped:") || !strings.Contains(texts[0], "over a day ago") ||
		!strings.HasPrefix(texts[1], "[Spent] stopped:") || !strings.Contains(texts[1], fmt.Sprintf("resumed it %d times", resumeMax)) {
		t.Fatalf("told: %q", texts)
	}
	if rec, _ := a.loadRecord("Resting"); rec.State != "ended" {
		t.Fatalf("Resting: %+v", rec)
	}
	if msgs, cut := a.recoverAgents(); len(msgs)+len(cut) != 0 {
		t.Fatalf("again: %v %v", msgs, cut)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("%d calls", n)
	}

	// Cut off past the cap, it is reported at once, as without auto-resume.
	report := reports(t, a)
	savedAgent(t, a, "Capped", agentRecord{State: "stopped", Resumes: resumeMax})
	a.Tell("Capped", "wait-for-stop")
	atWork(t, c, "Capped")
	a.quit()
	if got := report(); !strings.HasPrefix(got, "[Capped] stopped: opt-chat quit") || !strings.Contains(got, fmt.Sprintf("resumed it %d times", resumeMax)) {
		t.Fatalf("report: %q", got)
	}
	if rec, _ := a.loadRecord("Capped"); rec.Cut {
		t.Fatalf("Capped: %+v", rec)
	}
}

// A loop cut off mid-turn goes on with the turns it had: its cap holds
// across the restart.
func TestLoopTurnsSurviveResume(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	a.autoResume = true
	l := loopHookFor(t, `r="go on $n"; [ $n -eq 2 ] && r=wait-for-stop; echo '{"decision":"block","reason":"'"$r"'"}'`)
	l.Max = 3
	a.Spawn("Loop", "loop-task", "", "", 0, l)
	waitFor(t, "its second turn", func() bool { return strings.Contains(agentPlain(c.dir, "Loop"), "loop: wait-for-stop") })
	atWork(t, c, "Loop")
	time.Sleep(100 * time.Millisecond)
	a.quit()
	if rec, _ := a.loadRecord("Loop"); rec.Turns != 2 || !rec.Cut {
		t.Fatalf("its record: %+v", rec)
	}

	b, _ := fakeAgent(t, c)
	b.autoResume = true
	report := reports(t, b)
	_, cut := b.recoverAgents()
	b.resumeCut(cut)
	if got := report(); !strings.Contains(got, "loop stopped: 3 turns, its cap") {
		t.Fatalf("report: %q", got)
	}
	if n := len(hookInputs(t, l)); n != 3 {
		t.Fatalf("%d hook calls, want 3", n)
	}
	if rec, _ := b.loadRecord("Loop"); rec.Turns != 0 {
		t.Fatalf("after its report: %+v", rec)
	}
}

// reloadStub is a reloader whose build writes "new" and whose exec is
// recorded, returning err.
func reloadStub(t *testing.T, buildErr, execErr error) (*reloader, chan []string) {
	exe := filepath.Join(t.TempDir(), "opt-chat")
	if err := os.WriteFile(exe, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	execd := make(chan []string, 1)
	return &reloader{exe: exe, args: []string{exe, "-open=false"},
		build: func(out string) error {
			if buildErr != nil {
				os.WriteFile(out, []byte("half"), 0o700)
				return buildErr
			}
			return os.WriteFile(out, []byte("new"), 0o700)
		},
		exec: func(path string, argv, env []string) error {
			execd <- append([]string{path}, argv...)
			return execErr
		}}, execd
}

func content(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// A reload builds the new binary and puts it in place at once, waits for
// the turn underway to end, cuts the subagents at work (the next start
// resumes them, so the chat gets no report), and then execs it.
func TestReloadWaitsForTurn(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	building(t, c)
	h := newHarness(t, c)
	h.agent.autoResume = true
	r, execd := reloadStub(t, nil, nil)
	h.agent.Spawn("Worker", "wait-for-stop", "", "", 0, nil)
	atWork(t, c, "Worker")
	h.input(message{kind: "user", text: "wait-for-message"})
	waitFor(t, "the turn's tool call", func() bool { return hasLogged(c, `tool: Bash {"command":"wait"}`) })
	done := make(chan error, 1)
	go func() { done <- h.reload(r) }()
	waitFor(t, "the new binary", func() bool { return content(r.exe) == "new" })
	select {
	case argv := <-execd:
		t.Fatalf("exec before the turn ended: %q", argv)
	case <-time.After(300 * time.Millisecond):
	}
	h.input(message{kind: "user", text: "banana"})
	select {
	case argv := <-execd:
		if !slices.Equal(argv, []string{r.exe, r.exe, "-open=false"}) {
			t.Fatalf("exec %q", argv)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no exec")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !hasLogged(c, "opt: got: banana") {
		t.Fatalf("the turn: %q", logged(c))
	}
	if _, err := os.Stat(r.exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("left %s.new: %v", r.exe, err)
	}
	noneLogged(t, c, "stopped")
	if rec, _ := h.agent.loadRecord("Worker"); rec.State != "stopped" || !rec.Cut {
		t.Fatalf("Worker: %+v", rec)
	}
}

// A build that fails changes nothing: the binary stays, nothing stops, and
// the chat goes on.
func TestReloadBuildFails(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	h.agent.autoResume = true
	r, execd := reloadStub(t, errors.New("main.go:1: syntax error"), nil)
	h.agent.Spawn("Worker", "wait-for-stop", "", "", 0, nil)
	atWork(t, c, "Worker")
	if err := h.reload(r); err == nil || !strings.Contains(err.Error(), "main.go:1: syntax error") {
		t.Fatalf("reload: %v", err)
	}
	if got := content(r.exe); got != "old" {
		t.Fatalf("the binary: %q", got)
	}
	if _, err := os.Stat(r.exe + ".new"); !os.IsNotExist(err) {
		t.Fatalf("left %s.new: %v", r.exe, err)
	}
	if len(execd) != 0 {
		t.Fatal("exec")
	}
	h.agent.mu.Lock()
	p := h.agent.running["Worker"]
	h.agent.mu.Unlock()
	if p == nil {
		t.Fatal("Worker stopped")
	}
	h.input(message{kind: "user", text: "after"})
	waitFor(t, "a turn", func() bool {
		return slices.ContainsFunc(logged(c), func(m string) bool { return strings.HasPrefix(m, "opt: answer to") && strings.Contains(m, "after") })
	})
}

// An exec that fails keeps this opt-chat: the chat goes on, and the
// subagents it cut go on from their sessions, as at a start.
func TestReloadExecFails(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	h.agent.autoResume = true
	r, execd := reloadStub(t, nil, errors.New("exec format error"))
	h.agent.Spawn("Worker", "wait-for-stop", "", "", 0, nil)
	atWork(t, c, "Worker")
	if err := h.reload(r); err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("reload: %v", err)
	}
	if len(execd) != 1 {
		t.Fatal("no exec")
	}
	waitFor(t, "Worker resumed and reported", func() bool {
		return slices.ContainsFunc(logged(c), func(m string) bool {
			return strings.HasPrefix(m, "work: [Worker] answer to") && strings.Contains(m, "check its effect")
		})
	})
	h.input(message{kind: "user", text: "after"})
	waitFor(t, "a turn", func() bool {
		return slices.ContainsFunc(logged(c), func(m string) bool { return strings.HasPrefix(m, "opt: answer to") && strings.Contains(m, "after") })
	})
	if got := h.agent.Spawn("Next", "x", "", "", 0, nil); !strings.HasPrefix(got, "Started [Next]") {
		t.Fatal(got)
	}
}

// Quitting while a reload waits for the turn ends the reload: no exec.
func TestReloadThenQuit(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	r, execd := reloadStub(t, nil, nil)
	h.input(message{kind: "user", text: "wait-for-message"})
	waitFor(t, "the turn's tool call", func() bool { return hasLogged(c, `tool: Bash {"command":"wait"}`) })
	done := make(chan error, 1)
	go func() { done <- h.reload(r) }()
	waitFor(t, "the new binary", func() bool { return content(r.exe) == "new" })
	h.quit()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reload went on after a quit")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("reload hung")
	}
	if len(execd) != 0 {
		t.Fatal("exec after a quit")
	}
}

// One reload at a time.
func TestReloadOnce(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	r, _ := reloadStub(t, nil, nil)
	h.input(message{kind: "user", text: "wait-for-message"})
	waitFor(t, "the turn's tool call", func() bool { return hasLogged(c, `tool: Bash {"command":"wait"}`) })
	go h.reload(r)
	waitFor(t, "the new binary", func() bool { return content(r.exe) == "new" })
	if err := h.reload(r); err == nil || !strings.Contains(err.Error(), "underway") {
		t.Fatalf("a second reload: %v", err)
	}
}

// goBuild builds the folder of exe into out and checks that out starts
// with the run's flags; a failure says why.
func TestGoBuild(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	exe, out := filepath.Join(src, "x"), filepath.Join(src, "x.new")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n\ngo 1.24\n")
	write("main.go", `package main

import (
	"flag"
	"os"
)

func main() {
	flag.Bool("open", true, "")
	flag.Parse()
	if os.Getenv("OPT_CHAT_PROBE") != "" {
		return
	}
	select {}
}
`)
	if err := goBuild(exe, out, []string{exe, "-open=false"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode()&0o100 == 0 {
		t.Fatalf("built: %v %v", fi, err)
	}
	if err := goBuild(exe, out, []string{exe, "-gone=1"}); err == nil || !strings.Contains(err.Error(), "won't start") || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a flag it lacks: %v", err)
	}
	write("main.go", "package main\n\nfunc main() { nope }\n")
	if err := goBuild(exe, out, []string{exe}); err == nil || !strings.Contains(err.Error(), "main.go:3") {
		t.Fatalf("a broken build: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "x")
	if err := goBuild(empty, empty+".new", []string{empty}); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("no source: %v", err)
	}
}

// opt-chat itself passes the check: it stops after its flags when asked.
func TestGoBuildSelf(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("builds opt-chat")
	}
	wd, _ := os.Getwd()
	out := filepath.Join(t.TempDir(), "opt-chat.new")
	if err := goBuild(filepath.Join(wd, "opt-chat"), out, []string{"opt-chat", "-open=false", "-model=opus", "-dir=" + t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

// A reload runs again with the run's flags, less an import, and opens no
// browser.
func TestReloadArgs(t *testing.T) {
	t.Parallel()
	fs := flag.NewFlagSet("opt-chat", flag.ContinueOnError)
	fs.String("model", "fable", "")
	fs.String("import", "", "")
	fs.String("import-since", "", "")
	fs.Bool("open", true, "")
	fs.Bool("repl", false, "")
	fs.String("dir", "", "")
	if err := fs.Parse([]string{"-model", "opus", "-import", "/x.zip", "-import-since=2026-01-01", "-repl", "-dir", "/d"}); err != nil {
		t.Fatal(err)
	}
	if got, want := reloadArgs(fs), []string{"-dir=/d", "-model=opus", "-repl=true", "-open=false"}; !slices.Equal(got, want) {
		t.Fatalf("%q, want %q", got, want)
	}
}

// /reload asks for one where opt-chat can.
func TestReloadCommand(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	if _, ok, err := command(a, "/reload"); !ok || err == nil {
		t.Fatalf("without a reloader: %v %v", ok, err)
	}
	asked := 0
	a.onReload = func() { asked++ }
	if out, ok, err := command(a, "/reload"); !ok || err != nil || asked != 1 || !strings.Contains(out, "Reloading") {
		t.Fatalf("%q %v %v, asked %d", out, ok, err, asked)
	}
}

// After a crash, a subagent's Claude Code can still be at work, its turn
// going on without opt-chat: the start stops it before resuming, so two
// never share a session. A pid whose process names another session (the
// pid reused) is left alone.
func TestAutoResumeStopsOrphan(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	a.autoResume = true
	start := func(tag string) *exec.Cmd {
		cmd := exec.Command("sh", "-c", "sleep 30; : "+tag)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	savedAgent(t, a, "Orphan", agentRecord{State: "working"})
	rec, _ := a.loadRecord("Orphan")
	orphan, other := start(rec.Session), start("another-session")
	a.note("Orphan", func(r *agentRecord) { r.Pid = orphan.Process.Pid })
	savedAgent(t, a, "Reused", agentRecord{State: "working", Pid: other.Process.Pid})
	ended := make(chan error, 1)
	go func() { ended <- orphan.Wait() }()
	if _, cut := a.recoverAgents(); len(cut) != 2 {
		t.Fatalf("to resume: %v", cut)
	}
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("the orphan still runs")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("stopped a process of another session: %v", err)
	}
}
