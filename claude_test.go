package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake `claude -p`: run with OPT_FAKE_CLAUDE set,
// it speaks stream-json, answering each message by what it says.
func TestMain(m *testing.M) {
	if os.Getenv("OPT_FAKE_CLAUDE") != "" {
		fakeClaude()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const pngHead = "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"

func fakeClaude() {
	b, _ := json.Marshal(os.Args[1:])
	appendRaw(os.Getenv("OPT_FAKE_ARGS"), append(b, '\n'))
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		os.Stdout.Write(append(b, '\n'))
	}
	say := func(bs ...block) { emit(map[string]any{"type": "assistant", "message": map[string]any{"content": bs}}) }
	result := func(text string) {
		say(textBlock(text))
		emit(map[string]any{"type": "result", "result": text, "usage": map[string]any{"cache_read_input_tokens": 1000}})
	}
	lines := make(chan string)
	go func() {
		in := bufio.NewScanner(os.Stdin)
		in.Buffer(nil, 64<<20)
		for in.Scan() {
			lines <- in.Text()
		}
		close(lines)
	}()
	read := func(line string) (text string, images int) {
		var m struct {
			Message struct {
				Content []block `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal([]byte(line), &m)
		for _, b := range m.Message.Content {
			text += b.Text
			if b.Type == "image" {
				images++
			}
		}
		return text, images
	}
	failRetry := false
	for line := range lines {
		text, images := read(line)
		switch {
		case strings.HasSuffix(text, viewGoesOn):
			if strings.Contains(text, "slow-ack") {
				time.Sleep(300 * time.Millisecond)
			}
			result("ok")
		case strings.Contains(text, "Too long:") && failRetry:
			emit(map[string]any{"type": "result", "is_error": true, "result": "overloaded"})
		case strings.Contains(text, "Too long:"):
			if strings.Contains(text, "your line is 600 bytes") && strings.Contains(text, strings.Repeat("x", nodeMax)+"| ← LIMIT") {
				result("short line")
			} else {
				result("bad retry: " + text)
			}
		case strings.Contains(text, "Compaction:") && strings.Contains(text, "make-it-long"):
			failRetry = strings.Contains(text, "then-fail")
			result(strings.Repeat("x", 600))
		case strings.Contains(text, "Compaction:"):
			result(fmt.Sprintf("line with %d images", images))
		case strings.Contains(text, "wait-for-message"):
			say(textBlock("waiting"))
			next, _ := read(<-lines)
			result("got: " + next)
		case strings.Contains(text, "wait-for-stop"):
			say(textBlock("working"))
			<-stop
			os.Exit(143)
		default:
			say(block{Type: "tool_use", Name: "Read", Input: []byte(`{"file_path":"x.png"}`)})
			img, _ := json.Marshal([]block{{Type: "image", Source: &source{Type: "base64", MediaType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUg=="}}})
			emit(map[string]any{"type": "user", "message": map[string]any{"content": []block{{Type: "tool_result", Content: img}}}})
			result(fmt.Sprintf("answer to %q with %d images", text, images))
		}
	}
}

// fakeAgent runs its calls on fakeClaude, recording each call's arguments.
func fakeAgent(t *testing.T, c *Chat) (*Agent, func() [][]string) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	script := fmt.Sprintf("#!/bin/sh\nOPT_FAKE_CLAUDE=1 OPT_FAKE_ARGS=%q exec %q \"$@\"\n", argsFile, self)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	a := &Agent{chat: c, system: "system", mcpConfig: "turn-mcp", agentMCP: "agent-mcp", model: "big", amodel: "mid", cmodel: "small",
		perm: "auto", cwd: t.TempDir(), tools: "Read", running: map[string]*proc{}, writing: map[string]chan struct{}{},
		report: func(text string) { c.Log("work", text) }}
	a.life, a.stop = context.WithCancel(context.Background())
	t.Cleanup(a.quit)
	return a, func() [][]string {
		b, _ := os.ReadFile(argsFile)
		var calls [][]string
		for line := range strings.Lines(string(b)) {
			var args []string
			json.Unmarshal([]byte(line), &args)
			calls = append(calls, args)
		}
		return calls
	}
}

func flagOf(args []string, name string) string {
	if i := slices.Index(args, name); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// A turn: the view's first part answered "ok" and dropped, the message with
// its image after the rest, and every step logged, a tool result's image too.
func TestTurn(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	for i := range 70 {
		c.Log("note", fmt.Sprint("note ", i))
	}
	a, calls := fakeAgent(t, c)
	img, _ := c.saveImage([]byte(pngHead))
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi", images: []string{img}}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range c.msgs[70:] {
		got = append(got, fmt.Sprintf("%s: %s %d", m.Kind, m.Text, len(m.Images)))
	}
	rest := "<chat>\n64+1|note: note 64\n65+1|note: note 65\n66+1|note: note 66\n67+1|note: note 67\n68+1|note: note 68\n69+1|note: note 69\n</chat>"
	want := []string{"user: hi 1", `tool: Read {"file_path":"x.png"} 0`, "echo: [image] 1", fmt.Sprintf("opt: answer to %q with 1 images 0", rest+"hi")}
	if !slices.Equal(got, want) {
		t.Fatalf("logged:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	args := calls()[0]
	if flagOf(args, "--model") != "big" || flagOf(args, "--permission-mode") != "auto" || flagOf(args, "--mcp-config") != "turn-mcp" ||
		flagOf(args, "--tools") != "Read" || flagOf(args, "--system-prompt") != "system" || slices.Contains(args, "--settings") {
		t.Fatalf("turn args: %q", args)
	}
}

// A compaction: same system prompt and tools, every call blocked, and a
// line too long tried again with the cut.
func TestCompact(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	lines := make([]string, 70)
	for i := range lines {
		lines[i] = fmt.Sprintf("%d+1|note: %d", i, i)
	}
	got, err := a.Compact(context.Background(), lines, []block{textBlock("Compaction: make-it-long")})
	if err != nil || got != "short line" {
		t.Fatalf("Compact = %q, %v", got, err)
	}
	img, _ := c.saveImage([]byte(pngHead))
	got, err = a.Compact(context.Background(), nil, append([]block{textBlock("Compaction: x")}, c.imageBlocks([]string{img})...))
	if err != nil || got != "line with 1 images" {
		t.Fatalf("Compact = %q, %v", got, err)
	}
	args := calls()[0]
	if flagOf(args, "--model") != "small" || flagOf(args, "--permission-mode") != "default" || flagOf(args, "--mcp-config") != "turn-mcp" ||
		flagOf(args, "--tools") != "Read" || flagOf(args, "--system-prompt") != "system" || flagOf(args, "--settings") != noTools {
		t.Fatalf("compaction args: %q", args)
	}
	got, err = a.Compact(context.Background(), nil, []block{textBlock("Compaction: make-it-long then-fail")})
	if err != nil || got != strings.Repeat("x", 600) {
		t.Fatalf("a failed retry lost the line in hand: %q, %v", got, err)
	}
	if n, u := a.takeCompactions(); n != 3 || u.Read == 0 {
		t.Fatalf("compactions counted: %d, %v", n, u)
	}
}

// A message sent while a turn runs reaches it, and is logged once.
func TestMidRun(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	turn := &Turn{agent: a}
	done := make(chan error)
	go func() { done <- turn.Run(context.Background(), []message{{kind: "user", text: "wait-for-message"}}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		working := len(c.msgs) == 2
		c.mu.Unlock()
		if working {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !turn.Send(&message{kind: "user", text: "banana"}) {
		t.Fatal("the running turn took no message")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if turn.Send(&message{kind: "user", text: "late"}) {
		t.Fatal("a finished turn took a message")
	}
	var got []string
	for _, m := range c.msgs {
		got = append(got, m.Kind+": "+m.Text)
	}
	if !slices.Equal(got, []string{"user: wait-for-message", "opt: waiting", "user: banana", "opt: got: banana"}) {
		t.Fatalf("logged: %q", got)
	}
}

// A subagent: same tools from its own MCP address, its steps in its own log,
// its report back as one work message; quitting stops it and says so.
func TestSubagentsAndQuit(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	reported := make(chan string, 2)
	a.report = func(text string) { c.Log("work", text); reported <- text }
	if got := a.Spawn("Scout", "look around"); !strings.HasPrefix(got, "Started [Scout]") {
		t.Fatal(got)
	}
	report := func() string {
		select {
		case got := <-reported:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	if got, want := report(), fmt.Sprintf("[Scout] answer to %q with 0 images", "<chat>\n</chat>You are subagent [Scout]. Your task:\nlook around"); got != want {
		t.Fatalf("report %q, want %q", got, want)
	}
	if !strings.Contains(c.ZoomAgent("[Scout]"), `tool: Read {"file_path":"x.png"}`) {
		t.Fatalf("Scout's log: %q", c.ZoomAgent("Scout"))
	}
	if got := a.Spawn("Scout", "again"); !strings.HasPrefix(got, "Started [Scout-2]") || !strings.HasPrefix(report(), "[Scout-2] ") {
		t.Fatalf("a second Scout: %q", got)
	}
	if args := calls()[0]; flagOf(args, "--model") != "mid" || flagOf(args, "--mcp-config") != "agent-mcp" || flagOf(args, "--tools") != "Read" || slices.Contains(args, "--settings") {
		t.Fatalf("subagent args: %q", args)
	}

	a.Spawn("Sleeper", "wait-for-stop")
	deadline := time.Now().Add(10 * time.Second)
	for {
		a.mu.Lock()
		p := a.running["Sleeper"]
		a.mu.Unlock()
		if p != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Sleeper never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := a.Tell("[Sleeper]", "hurry"); got != "Sent." {
		t.Fatal(got)
	}
	quit := make(chan struct{})
	go func() { a.quit(); close(quit) }()
	select {
	case <-quit:
	case <-time.After(10 * time.Second):
		t.Fatal("quit hung")
	}
	if last := c.msgs[len(c.msgs)-1]; last.Text != "[Sleeper] stopped: opt-chat quit before this subagent finished." {
		t.Fatalf("last message: %q", last.Text)
	}
	if !strings.Contains(c.ZoomAgent("Sleeper"), "opt: hurry") {
		t.Fatalf("Sleeper's log: %q", c.ZoomAgent("Sleeper"))
	}
	if got := a.Spawn("Late", "x"); got != "opt-chat is quitting." {
		t.Fatal(got)
	}
}

// Calls drop the launching Claude Code session's variables and run without
// background shells.
func TestChildEnv(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s")
	t.Setenv("CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "0")
	t.Setenv("OPT_KEEP", "yes")
	env := childEnv()
	has := func(kv string) bool { return slices.Contains(env, kv) }
	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=")
	}) || has("CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=0") || !has("CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1") || !has("OPT_KEEP=yes") {
		t.Fatalf("childEnv: %q", env)
	}
}

// A message that comes while the view's first part is being answered is
// taken by the turn, and sent right after its input.
func TestSendDuringAck(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("note", "slow-ack")
	for i := range 69 {
		c.Log("note", fmt.Sprint("note ", i))
	}
	a, _ := fakeAgent(t, c)
	turn := &Turn{agent: a}
	done := make(chan error)
	go func() { done <- turn.Run(context.Background(), []message{{kind: "user", text: "hi"}}) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		turn.mu.Lock()
		acking := turn.open && turn.p == nil
		turn.mu.Unlock()
		if acking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never started")
		}
		time.Sleep(time.Millisecond)
	}
	if !turn.Send(&message{kind: "user", text: "early"}) {
		t.Fatal("the turn took no message during its ack")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range c.msgs[70:] {
		got = append(got, m.Kind+": "+m.Text)
	}
	if len(got) < 2 || got[0] != "user: hi" || got[1] != "user: early" || !strings.Contains(got[len(got)-1], `answer to "early"`) {
		t.Fatalf("logged: %q", got)
	}
}

func TestAgentName(t *testing.T) {
	for in, want := range map[string]string{"[Scout]": "Scout", "My agent": "My-agent", " [a/b] ": "a-b", "[]": "", "x_y-2": "x_y-2"} {
		if got := agentName(in); got != want {
			t.Errorf("agentName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The harness: a message starts a turn, one sent meanwhile joins it, and
// quit logs what still waits.
func TestHarness(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	h := &harness{agent: a}
	a.report = func(text string) { h.input(message{kind: "work", text: text}) }
	h.input(message{kind: "user", text: "wait-for-message"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		c.mu.Lock()
		n := len(c.msgs)
		c.mu.Unlock()
		if n == 2 { // its "waiting"
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.input(message{kind: "user", text: "banana"})
	for {
		h.mu.Lock()
		idle := !h.running
		h.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the turn never ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.quit()
	h.input(message{kind: "user", text: "after quit"})
	var got []string
	for _, m := range c.msgs {
		got = append(got, m.Kind+": "+m.Text)
	}
	if !slices.Equal(got, []string{"user: wait-for-message", "opt: waiting", "user: banana", "opt: got: banana", "user: after quit"}) {
		t.Fatalf("logged: %q", got)
	}
}
