package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// bigAgentLog writes the log of a subagent that ran long: n tool calls,
// each with an echo of size bytes.
func bigAgentLog(t *testing.T, c *Chat, name string, n, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("task: fix the build\n\non: opus medium\n\n")
	for i := range n {
		fmt.Fprintf(&b, "tool@%d#toolu_%d: Bash {\"command\":\"make test %d\"}\n\n", 1761134400000+i, i, i)
		fmt.Fprintf(&b, "echo@%d#toolu_%d: result %d %s\n\n", 1761134400000+i, i, i, strings.Repeat("x", size))
	}
	b.WriteString("agent@1761134499999: done: the build is green\n\n")
	if err := os.WriteFile(filepath.Join(c.dir, "agents", name+".log"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// memAgent is an Agent on c with subagents' memory on, as by default.
func memAgent(c *Chat) *Agent {
	return &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}, mems: newAgentMems(c, defaultAgentJobs)}
}

func zoomTool(a *Agent, id any, n int) string {
	b, _ := json.Marshal(id)
	s, _ := callTool(a, false, "zoom", toolArgs{ID: b, N: n})
	return s
}

// zoom("Name") on a subagent that ran long gives a bounded view of its
// chat, not its whole log, which overflowed the caller's context.
func TestAgentZoomBounded(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	bigAgentLog(t, c, "Big", 300, 5000) // 1.5 MB
	got := zoomTool(a, "Big", 0)
	if len(got) > 40_000 {
		t.Fatalf("zoom(\"Big\") gave %d bytes", len(got))
	}
}

// A subagent's chat is summarized as the chat is: its view's lines are
// built, zoom("Name:id", n) opens them down to a whole step, a running
// subagent's new steps go in as it writes them, and a chat opened again
// after steps it missed takes each once.
func TestAgentMemTree(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 300)
	a := memAgent(c)
	bigAgentLog(t, c, "Big", 300, 5000)
	a.mems.setCompact(c.compact)
	sc, err := a.mems.chat("Big")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sc)
	steps := 2 + 2*300 + 1
	view := zoomTool(a, "Big", 0)
	if !strings.Contains(view, fmt.Sprintf("[Big]'s chat, %d messages, as lines", steps)) || strings.Contains(view, unbuilt) {
		t.Fatalf("view: %.300q", view)
	}
	if len(view) > agentViewHi+1000 {
		t.Fatalf("view of %d bytes", len(view))
	}
	if got := zoomTool(a, "Big:2", 1); got != "2+1|tool: Bash {\"command\":\"make test 0\"}" {
		t.Fatalf("a step: %q", got)
	}
	first := strings.SplitN(strings.SplitN(view, "\n", 3)[1], "|", 2)[0] // the view's first line, "0+n"
	id, n := 0, 0
	fmt.Sscanf(first, "%d+%d", &id, &n)
	if n < 2 {
		t.Fatalf("the view's first line %q merges nothing", first)
	}
	if got := zoomTool(a, fmt.Sprintf("Big:%d", id), n); !strings.HasPrefix(got, fmt.Sprintf("0+%d|", n/2)) || strings.Count(got, "\n") != 1 {
		t.Fatalf("zoom(Big:0, %d): %.200q", n, got)
	}

	a.agentWrite("Big", "agent", "one more step")
	if got := zoomTool(a, fmt.Sprintf("Big:%d", steps), 1); got != fmt.Sprintf("%d+1|agent: one more step", steps) {
		t.Fatalf("a running subagent's step: %q", got)
	}

	// As after a crash between its log and its chat: the chat is closed,
	// and a step is logged that it never got.
	waitIdle(t, sc)
	a.mems.mu.Lock()
	sc.lock.Close()
	a.mems.open = nil
	a.mems.mu.Unlock()
	f, _ := os.OpenFile(filepath.Join(c.dir, "agents", "Big.log"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("thinking: hidden\n\nagent: missed\n\n")
	f.Close()
	sc, err = a.mems.chat("Big")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sc)
	sc.mu.Lock()
	got := len(sc.msgs)
	sc.mu.Unlock()
	if got != steps+2 {
		t.Fatalf("%d messages after the reopen, want %d", got, steps+2)
	}
	if got := zoomTool(a, fmt.Sprintf("Big:%d", steps+1), 1); got != fmt.Sprintf("%d+1|agent: missed", steps+1) {
		t.Fatalf("the missed step: %q", got)
	}

	// An entry being written goes in once it ends.
	appendLog := func(s string) {
		f, _ := os.OpenFile(filepath.Join(c.dir, "agents", "Big.log"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString(s)
		f.Close()
	}
	half := fmt.Sprintf("Big:%d", steps+2)
	appendLog("agent: half")
	if got := zoomTool(a, half, 1); !strings.HasPrefix(got, "No line") {
		t.Fatalf("an entry half written: %q", got)
	}
	appendLog(" and the rest\n\n")
	if got := zoomTool(a, half, 1); got != fmt.Sprintf("%d+1|agent: half and the rest", steps+2) {
		t.Fatalf("the entry once whole: %q", got)
	}
}

// With -agent-memory=false, zoom("Name") gives the whole log as before,
// steps go to the log only, and the prompt and the zoom tool say so.
func TestAgentMemOff(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	bigAgentLog(t, c, "Big", 3, 10)
	a.agentWrite("Big", "agent", "one more step")
	got := zoomTool(a, "Big", 0)
	if want := agentPlain(c.dir, "Big"); got != want || !strings.HasPrefix(got, "task: fix the build\n\non: opus medium\n\ntool: Bash") || !strings.Contains(got, "agent: one more step") {
		t.Fatalf("zoom(\"Big\"): %.200q", got)
	}
	if got := zoomTool(a, "Big:0", 1); !strings.Contains(got, "-agent-memory=false") {
		t.Fatalf("zoom(\"Big:0\", 1): %q", got)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "agents", "Big.mem")); !os.IsNotExist(err) {
		t.Fatalf("a chat made for Big: %v", err)
	}
	for _, mem := range []bool{true, false} {
		a.mems = nil
		if mem {
			a.mems = newAgentMems(c, 1)
		}
		var zoom string
		for _, tl := range a.mcpList() {
			if tl["name"] == "zoom" {
				zoom = tl["description"].(string)
			}
		}
		prompt := systemPrompt("", "", false, mem)
		if lines := strings.Contains(zoom, `zoom("Name:id", n)`); lines != mem || strings.Contains(prompt, `zoom("Name:id", n)`) != mem {
			t.Errorf("memory %v: the zoom tool says %q; the prompt offers lines: %v", mem, zoom, !lines)
		}
		if whole := strings.HasPrefix(a.chatRef("Big"), "Full chat"); whole == mem {
			t.Errorf("memory %v: a report ends %q", mem, a.chatRef("Big"))
		}
	}
}

// A zoom while a step is half written waits for the write, which holds the
// subagent's lock: it never takes the half for whole entries, as a blank
// line in the step's text would make it look, and so never cuts the step.
func TestAgentMemTornWrite(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("W", "task", "write steps")
	text := "first half\n\nsecond half"
	b := []byte(stepHead(step{Kind: "echo", Text: text, At: nowMs()}) + text + "\n\n")
	cut := bytes.Index(b, []byte("\n\nsecond")) + 2
	am := a.mems.lock("W") // as appendStep holds it over its write
	if err := appendRaw(a.agentLog("W"), b[:cut]); err != nil {
		t.Fatal(err)
	}
	zoomed := make(chan struct{})
	go func() { a.ZoomAgent("W"); close(zoomed) }()
	select {
	case <-zoomed:
		t.Fatal("zoom read the log while a step was half written")
	case <-time.After(200 * time.Millisecond):
	}
	if err := appendRaw(a.agentLog("W"), b[cut:]); err != nil {
		t.Fatal(err)
	}
	am.mu.Unlock()
	<-zoomed
	if got := strings.Join(memTexts(t, a, "W"), ","); got != "write steps,"+text {
		t.Fatalf("the chat: %q", got)
	}
}

// The first zoom of a subagent from before, nothing summarized yet, shows
// what it was asked and what it ended with, not bare placeholders.
func TestAgentZoomFirstViewHeads(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	bigAgentLog(t, c, "Big", 300, 5000)
	view := zoomTool(a, "Big", 0)
	for _, want := range []string{"task: fix the build", "agent: done: the build is green"} {
		if !strings.Contains(view, want) {
			t.Errorf("first view lacks %q: %.400q", want, view)
		}
	}
	if len(view) > agentViewHi+1000 {
		t.Errorf("view of %d bytes", len(view))
	}
}

// Subagents' compactions share one pool of -agent-jobs, and none starts
// while the main chat has a line waiting to be built.
func TestAgentMemSharesQueue(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	mainFake := c.compact
	c.mu.Lock()
	c.compact = func(ctx context.Context, l []string, s int, b []block) (string, error) {
		time.Sleep(2 * time.Millisecond)
		return mainFake(ctx, l, s, b)
	}
	c.mu.Unlock()
	var now, most atomic.Int64
	var early atomic.Int64
	sub := func(ctx context.Context, l []string, s int, b []block) (string, error) {
		if v := now.Add(1); v > most.Load() {
			most.Store(v)
		}
		defer now.Add(-1)
		c.mu.Lock()
		if _, ok := c.next(); ok {
			early.Add(1)
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
		return strings.Repeat("s", 100), nil
	}
	a.mems.setCompact(sub)
	var ms []Msg
	for i := range 400 {
		ms = append(ms, Msg{Kind: "user", Text: fmt.Sprintf("message %d %s", i, strings.Repeat("y", 800))})
	}
	c.logMsgs(ms)
	for _, name := range []string{"A", "B", "C"} {
		bigAgentLog(t, c, name, 40, 2000)
		zoomTool(a, name, 0)
	}
	waitIdle(t, c)
	for _, name := range []string{"A", "B", "C"} {
		sc, _ := a.mems.chat(name)
		waitIdle(t, sc)
	}
	if most.Load() > int64(a.mems.gate.max) {
		t.Errorf("%d subagent compactions at once, want at most %d in all", most.Load(), a.mems.gate.max)
	}
	if early.Load() > 0 {
		t.Errorf("%d subagent compactions started while the main chat had lines waiting", early.Load())
	}
}

// A first zoom of one subagent, however long its log takes to read, holds
// up no other subagent's steps. Old's log is a pipe: its zoom stays in the
// read, holding whatever it holds, until the test writes the log and closes
// it.
func TestAgentMemNoGlobalStall(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("Run", "task", "work")
	pipe := filepath.Join(c.dir, "agents", "Old.log")
	if err := syscall.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	zoomed := make(chan string)
	go func() { zoomed <- a.ZoomAgent("Old") }()
	w, err := os.OpenFile(pipe, os.O_WRONLY, 0) // returns once the zoom opened it to read
	if err != nil {
		t.Fatal(err)
	}
	stepped := make(chan struct{})
	go func() {
		a.agentWrite("Run", "agent", "a step")
		close(stepped)
	}()
	select {
	case <-stepped:
	case got := <-zoomed:
		t.Fatalf("the zoom ended before its log did: %q", got)
	case <-time.After(time.Minute): // a deadlock, not a slow machine
		w.Close()
		t.Fatal("a step of another subagent waited on Old's first zoom")
	}
	w.WriteString("task: old work\n\nagent: done\n\n")
	w.Close()
	if got := <-zoomed; !strings.Contains(got, "[Old]'s chat, 2 messages") {
		t.Fatalf("zoom(\"Old\"): %q", got)
	}
	if got := zoomTool(a, "Run:1", 1); got != "1+1|agent: a step" {
		t.Fatalf("Run's step: %q", got)
	}
}

// A subagent chat's failed compaction says so on the page, naming the
// subagent.
func TestAgentMemFailureNoticed(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.hub = newHub()
	var mu sync.Mutex
	var notes []string
	c.hub.print = func(v any) {
		if n, ok := v.(noticeUpdate); ok {
			mu.Lock()
			notes = append(notes, n.S)
			mu.Unlock()
		}
	}
	a := memAgent(c)
	a.mems.setCompact(func(context.Context, []string, int, []block) (string, error) {
		return "", fmt.Errorf("model down")
	})
	bigAgentLog(t, c, "Bad", 5, 2000)
	zoomTool(a, "Bad", 0)
	defer a.mems.setCompact(nil)
	waitFor(t, "a notice naming [Bad]", func() bool {
		mu.Lock()
		defer mu.Unlock()
		got := strings.Join(notes, "\n")
		return strings.Contains(got, "[Bad]") && strings.Contains(got, "model down")
	})
}

// Subagents' chats don't stay open for the life of the process: past
// agentOpenMax, idle ones close, and one closed opens again whole.
func TestAgentMemCloses(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	a.mems.setCompact(c.compact)
	const n = agentOpenMax + 8
	for i := range n {
		name := fmt.Sprintf("S%d", i)
		bigAgentLog(t, c, name, 3, 100)
		zoomTool(a, name, 0)
		sc, _ := a.mems.chat(name)
		waitIdle(t, sc)
	}
	a.mems.mu.Lock()
	open := len(a.mems.open)
	a.mems.mu.Unlock()
	if open > agentOpenMax {
		t.Fatalf("%d subagents' chats open, want at most %d", open, agentOpenMax)
	}
	a.agentWrite("S0", "agent", "after the close")
	if got := zoomTool(a, "S0:9", 1); got != "9+1|agent: after the close" {
		t.Fatalf("a closed chat opened again: %q", got)
	}
	sc, _ := a.mems.chat("S0")
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.msgs) != 10 {
		t.Fatalf("%d messages after the reopen, want 10", len(sc.msgs))
	}
}

// A main chat line that keeps failing holds up no subagent's memory.
func TestAgentMemMainFailing(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	mainFake := c.compact
	var poisoned atomic.Bool
	poisoned.Store(true)
	c.mu.Lock()
	c.compact = func(ctx context.Context, l []string, s int, b []block) (string, error) {
		if poisoned.Load() && strings.Contains(b[0].Text, "poison") {
			return "", fmt.Errorf("refused")
		}
		return mainFake(ctx, l, s, b)
	}
	c.mu.Unlock()
	a.mems.setCompact(func(context.Context, []string, int, []block) (string, error) {
		return strings.Repeat("s", 100), nil
	})
	c.logMsgs([]Msg{{Kind: "user", Text: "poison " + strings.Repeat("p", 800)}})
	waitFor(t, "the main chat's line to fail and wait to be tried again", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.failing) > 0 && len(c.busy) == 0
	})
	bigAgentLog(t, c, "Sub", 5, 2000)
	sc, err := a.mems.chat("Sub")
	if err != nil {
		t.Fatal(err)
	}
	waitIdle(t, sc)
	poisoned.Store(false)
	c.logMsgs([]Msg{{Kind: "user", Text: "back"}}) // the user is back: it is tried again now
}

// stalls fails the test if step, a subagent's step or Opt's zoom, waits
// while another subagent's chat holds its lock, as its fsyncs do
// (AgentMem.tla, NoWaitOnOthersIO).
func stalls(t *testing.T, held *Chat, what string, step func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { step(); close(done) }()
	select {
	case <-done:
		held.mu.Unlock()
	case <-time.After(10 * time.Second): // a wait on another's lock, not a slow machine
		held.mu.Unlock()
		<-done
		t.Fatal(what + " waited on another subagent's chat")
	}
}

// The pump that follows any subagent's compaction never holds every
// subagent up behind one chat writing to disk.
func TestAgentMemPumpNoStall(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("A", "task", "work a")
	a.agentWrite("B", "task", "work b")
	scA, _ := a.mems.chat("A")
	scA.mu.Lock()
	go a.mems.pump() // jobGate.kick
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if !a.mems.mu.TryLock() {
			break // the pump is in
		}
		a.mems.mu.Unlock()
	}
	stalls(t, scA, "B's step", func() { a.agentWrite("B", "agent", "a step") })
	stalls(t, scA, "zoom(\"B\")", func() { scA.mu.Lock(); go a.mems.pump(); a.ZoomAgent("B") })
}

// Closing idle chats past agentOpenMax skips one that is writing, as it
// skips one being fed, and holds no other subagent up.
func TestAgentMemCloseNoStall(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	for i := range agentOpenMax {
		a.agentWrite(fmt.Sprintf("S%d", i), "task", "work")
	}
	s0, _ := a.mems.chat("S0") // the least used: the first closeIdle tries
	for i := 1; i < agentOpenMax; i++ {
		a.mems.chat(fmt.Sprintf("S%d", i))
	}
	s0.mu.Lock() // as Chat.build holds it over flushNodes and saveView
	stalls(t, s0, "a new subagent's first step", func() { a.agentWrite("New", "task", "work") })
	a.mems.mu.Lock()
	defer a.mems.mu.Unlock()
	if len(a.mems.open) > agentOpenMax {
		t.Fatalf("%d chats open, want at most %d", len(a.mems.open), agentOpenMax)
	}
}

// A step written after a crash cut the log's last one short is a step of
// its own, not the cut one's text (AgentMem.tla, NoLostStep).
func TestAgentMemTornTail(t *testing.T) {
	t.Parallel()
	for _, cut := range []string{"tool@1761134400001#toolu_1: Bash {\"comm", "too"} {
		c, _ := fakeChat(t, t.TempDir(), 100)
		os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
		os.WriteFile(filepath.Join(c.dir, "agents", "T.log"), []byte("task: work\n\n"+cut), 0o600)
		a := memAgent(c) // opt-chat started again
		a.agentWrite("T", "agent", "after the crash")
		sc, err := a.mems.chat("T")
		if err != nil {
			t.Fatal(err)
		}
		sc.mu.Lock()
		last := sc.msgs[len(sc.msgs)-1]
		sc.mu.Unlock()
		if last.Kind != "agent" || last.Text != "after the crash" {
			t.Errorf("cut %q: the last message is %s: %q", cut, last.Kind, last.Text)
		}
	}
}

// A subagent's memory that can't open says so on the page and in zoom,
// never that the subagent doesn't exist (AgentMem.tla, FailureSurfaces).
func TestAgentMemOpenFails(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.hub = newHub()
	var mu sync.Mutex
	var notes []string
	c.hub.print = func(v any) {
		if n, ok := v.(noticeUpdate); ok {
			mu.Lock()
			notes = append(notes, n.S)
			mu.Unlock()
		}
	}
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	os.WriteFile(filepath.Join(c.dir, "agents", "X.mem"), []byte("not a chat"), 0o600)
	a.agentWrite("X", "task", "work")
	mu.Lock()
	got := strings.Join(notes, "\n")
	mu.Unlock()
	if !strings.Contains(got, "[X] memory") {
		t.Errorf("no notice of X's memory failing: %q", got)
	}
	if z := zoomTool(a, "X", 0); strings.Contains(z, "No subagent") || !strings.Contains(z, "[X]") {
		t.Errorf("zoom(\"X\"): %q", z)
	}
}

// memDay is the day file a subagent's chat logs its messages to today.
func memDay(c *Chat, name string) string {
	return filepath.Join(c.dir, "agents", name+".mem", "main", time.Now().Format(time.DateOnly)+".jsonl")
}

// closeMem closes the subagent's chat as opt-chat's exit would, so another
// agentMems can open its files.
func closeMem(t *testing.T, a *Agent, name string) {
	t.Helper()
	am := a.mems.lock(name)
	if am.c != nil {
		am.c.lock.Close()
	}
	am.mu.Unlock()
}

// memTexts is the subagent's chat's messages, each step's parts joined.
func memTexts(t *testing.T, a *Agent, name string) []string {
	t.Helper()
	sc, err := a.mems.chat(name)
	if err != nil {
		t.Fatal(err)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	var out []string
	for _, m := range sc.msgs {
		if m.Part == 0 {
			out = append(out, m.Text)
		} else {
			out[len(out)-1] += m.Text
		}
	}
	return out
}

// With -agent-memory=false a step after one cut short still starts a step
// of its own, so the chat fed from the log once memory is on again has it
// (AgentMem.tla, NoLostStep, "offseal").
func TestAgentMemOffSeals(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	os.WriteFile(filepath.Join(c.dir, "agents", "O.log"), []byte("task: work\n\ntool@1761134400001#toolu_1: Bash {\"comm"), 0o600)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	a.agentWrite("O", "agent", "after the crash")
	a.mems = newAgentMems(c, 1) // a restart with memory on
	got := memTexts(t, a, "O")
	if len(got) == 0 || got[len(got)-1] != "after the crash" {
		t.Errorf("the chat ends %q", got)
	}
}

// A crash or a full disk in the write of a long step's parts leaves some of
// them on disk; the chat opened again ends the step, not skips the rest
// (AgentMem.tla, Converges, "parts").
func TestAgentMemTornParts(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("B", "task", "work")
	long := strings.Repeat("0123456789\n", 7000) // 77,000 characters: three parts
	a.agentWrite("B", "echo", long)
	closeMem(t, a, "B")
	f := memDay(c, "B")
	b, _ := os.ReadFile(f)
	lines := strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
	os.WriteFile(f, []byte(strings.Join(lines[:len(lines)-1], "")), 0o600) // the last part lost
	a = memAgent(c)
	a.agentWrite("B", "agent", "done")
	if got := memTexts(t, a, "B"); len(got) != 3 || got[1] != long || got[2] != "done" {
		t.Errorf("%d steps, the echo %d of %d bytes", len(got), len(got[min(1, len(got)-1)]), len(long))
	}
}

// A failed write to a subagent's chat, from a read-only file or a full
// disk, stops that chat's writes, not opt-chat:
// zoom gives the whole log meanwhile, and the chat catches up once the disk
// takes writes again. Run in a child process, as a fatal would end the test
// binary.
func TestAgentMemWriteFails(t *testing.T) {
	t.Parallel()
	breaks := map[string]func(day string) (mend func()){
		"read-only": func(day string) func() {
			os.Chmod(day, 0o400)
			return func() { os.Chmod(day, 0o600) }
		},
		"disk full": func(day string) func() {
			// A file size limit fails a write as a full disk does: what fits
			// is written, then an error (EFBIG here, ENOSPC there).
			fi, err := os.Stat(day)
			if err != nil {
				panic(err)
			}
			signal.Ignore(syscall.SIGXFSZ)
			var was syscall.Rlimit
			syscall.Getrlimit(syscall.RLIMIT_FSIZE, &was)
			lim := was
			lim.Cur = uint64(fi.Size()) + 7
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
				panic(err)
			}
			return func() { syscall.Setrlimit(syscall.RLIMIT_FSIZE, &was) }
		},
	}
	if dir, how := os.Getenv("AGENTMEM_CHILD"), os.Getenv("AGENTMEM_BREAK"); dir != "" {
		c, _ := fakeChat(t, dir, 100)
		c.hub = newHub()
		var mu sync.Mutex
		var notes []string
		c.hub.print = func(v any) {
			if n, ok := v.(noticeUpdate); ok {
				mu.Lock()
				notes = append(notes, n.S)
				mu.Unlock()
			}
		}
		a := memAgent(c)
		os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
		a.agentWrite("W", "task", "work")
		mend := breaks[how](memDay(c, "W"))
		a.agentWrite("W", "agent", "one")
		// The step's write cut the chat's day file short; opening it again
		// fails too while the disk is full, and zoom says why.
		if z := zoomTool(a, "W", 0); !strings.Contains(z, "agent: one") && !strings.Contains(z, "memory failed") {
			t.Errorf("%s: zoom(\"W\") with its memory failing: %q", how, z)
		}
		mend()
		a.agentWrite("W", "agent", "two")
		if got := strings.Join(memTexts(t, a, "W"), ","); got != "work,one,two" {
			t.Errorf("%s: the chat once the disk takes writes: %q", how, got)
		}
		mu.Lock()
		if !strings.Contains(strings.Join(notes, "\n"), "[W] memory") {
			t.Errorf("%s: no notice of W's memory failing: %q", how, notes)
		}
		mu.Unlock()
		fmt.Println("AGENTMEM_ALIVE")
		return
	}
	for how := range breaks {
		if how == "read-only" && os.Geteuid() == 0 {
			continue // root writes to a read-only file
		}
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestAgentMemWriteFails$", "-test.v")
			cmd.Env = append(os.Environ(), "AGENTMEM_CHILD="+t.TempDir(), "AGENTMEM_BREAK="+how)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "AGENTMEM_ALIVE") {
				t.Errorf("the child: %v\n%s", err, out)
			}
		})
	}
}

// A name whose log is gone but whose chat stays is not given again: a new
// subagent under it would have its steps taken for the old chat's.
func TestAgentMemNameReuse(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("N", "task", "old")
	closeMem(t, a, "N")
	os.Remove(filepath.Join(c.dir, "agents", "N.log"))
	a = memAgent(c)
	if name := a.freeName("N"); name == "N" {
		t.Errorf("spawn gives the name N again, with N.mem left")
	}
}

// While a subagent's chat can't be written, its steps warn once, not each
// one; once a write works, the next failure warns again.
func TestAgentMemFailWarnsOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only file")
	}
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.hub = newHub()
	var n atomic.Int32
	c.hub.print = func(v any) {
		if x, ok := v.(noticeUpdate); ok && strings.Contains(x.S, "[W] memory") {
			n.Add(1)
		}
	}
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("W", "task", "work")
	day := memDay(c, "W")
	os.Chmod(day, 0o400)
	for range 10 {
		a.agentWrite("W", "agent", "broken")
	}
	if got := n.Load(); got != 1 {
		t.Errorf("notices for 10 steps while W's memory fails: %d, want 1", got)
	}
	os.Chmod(day, 0o600)
	a.agentWrite("W", "agent", "mended")
	os.Chmod(day, 0o400)
	defer os.Chmod(day, 0o600)
	a.agentWrite("W", "agent", "broken again")
	if got := n.Load(); got != 2 {
		t.Errorf("notices once it failed again after a write that worked: %d, want 2", got)
	}
}
