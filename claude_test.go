package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
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
	base := os.Getenv("OPT_FAKE_ARGS")
	if len(os.Args) > 2 && os.Args[1] == "plugin" && os.Args[2] == "list" { // as Claude Code: one installed for two folders
		fmt.Println(`[{"id":"plug@market","scope":"user","enabled":true,"projectEnabled":false},
			{"id":"other@market","scope":"local","enabled":true,"projectEnabled":false},
			{"id":"other@market","scope":"local","enabled":false,"projectEnabled":false}]`)
		return
	}
	b, _ := json.Marshal(os.Args[1:])
	appendRaw(base, append(b, '\n'))
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDE") || strings.HasPrefix(kv, "ENABLE_TOOL_SEARCH") {
			env = append(env, kv)
		}
	}
	b, _ = json.Marshal(env)
	appendRaw(base+".env", append(b, '\n'))
	if f := os.Getenv("OPT_FAKE_PGID"); f != "" {
		appendRaw(f, []byte(fmt.Sprintln(syscall.Getpgrp())))
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		os.Stdout.Write(append(b, '\n'))
	}
	say := func(bs ...block) { emit(map[string]any{"type": "assistant", "message": map[string]any{"content": bs}}) }
	stream := func(ev any) { emit(map[string]any{"type": "stream_event", "event": ev}) }
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
			if b.Type == "image" || b.Type == "document" {
				images++
			}
		}
		return text, images
	}
	statusAsks, mcpOff := 0, map[string]bool{}
	// control answers a control request as Claude Code would; false for
	// any other line.
	control := func(line string) bool {
		var r struct {
			Type      string         `json:"type"`
			RequestID string         `json:"request_id"`
			Request   map[string]any `json:"request"`
		}
		json.Unmarshal([]byte(line), &r)
		if r.Type != "control_request" {
			return r.Type == "control_response"
		}
		ok := func(v any) {
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": r.RequestID, "response": v}})
		}
		b, _ := json.Marshal(r.Request)
		appendRaw(base+".ctl", append(b, '\n'))
		switch name, _ := r.Request["serverName"].(string); r.Request["subtype"] {
		case "mcp_status": // alpha connects on the third ask; beta is off once turned off
			statusAsks++
			alpha := "pending"
			if statusAsks >= 3 {
				alpha = "connected"
			}
			beta := "connected"
			if mcpOff["beta"] {
				beta = "disabled"
			}
			ok(map[string]any{"mcpServers": []map[string]any{
				{"name": "opt", "status": "connected"},
				{"name": "alpha", "status": alpha, "scope": "user", "source": "user", "config": map[string]any{"headers": map[string]any{"Authorization": "Bearer SECRET"}}},
				{"name": "beta", "status": beta, "scope": "claudeai", "source": "claudeai"},
			}})
		case "mcp_toggle":
			if name != "alpha" && name != "beta" {
				emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": r.RequestID, "error": "Server not found: " + name}})
				break
			}
			mcpOff[name] = r.Request["enabled"] == false
			ok(nil)
		case "mcp_authenticate":
			ok(map[string]any{"authUrl": "https://auth.example/" + name, "requiresUserAction": true})
		case "get_skills_dialog":
			ok(map[string]any{"skills": []map[string]any{
				{"name": "one", "display_name": "one", "description": "the first", "source": "user", "tokens": 10, "state": "on"},
				{"name": "plug:two", "display_name": "plug:two", "description": "the second", "source": "plugin", "tokens": 20, "state": "on", "locked_by": "plugin"},
			}})
		case "list_models":
			ok(map[string]any{"models": []map[string]any{
				{"value": "default", "displayName": "Default (recommended)", "description": "Big 2 · best", "supportsEffort": true},
				{"value": "big", "displayName": "Big 2", "description": "best", "supportsEffort": true, "supportedEffortLevels": []string{"low", "high"}},
				{"value": "claude-big-1", "displayName": "Big 1", "description": "older"},
			}})
		case "fail":
			emit(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": r.RequestID, "error": "no such thing"}})
		default:
			ok(map[string]any{})
		}
		return true
	}
	failRetry, longTries := false, -1
	for line := range lines {
		if control(line) {
			continue
		}
		text, images := read(line)
		in := text // what follows the view: triggers in the view are of turns before
		if i := strings.LastIndex(text, "</chat>"); i >= 0 {
			in = text[i:]
		}
		switch {
		case strings.HasSuffix(text, viewGoesOn):
			if strings.Contains(text, "slow-ack") {
				time.Sleep(300 * time.Millisecond)
			}
			result("ok")
		case strings.Contains(in, "Too long:") && longTries >= 0: // each try a byte shorter, a head echoed
			longTries++
			result(fmt.Sprintf("3+1|%s", strings.Repeat("y", 700-longTries)))
		case strings.Contains(in, "Too long:") && failRetry:
			emit(map[string]any{"type": "result", "is_error": true, "result": "overloaded"})
		case strings.Contains(in, "Too long:"):
			if strings.Contains(text, "your line is 600 bytes") && strings.Contains(text, strings.Repeat("x", nodeMax)+"| ← LIMIT") {
				result("short line")
			} else {
				result("bad retry: " + text)
			}
		case strings.Contains(in, "Compaction:") && strings.Contains(in, "empty-error"):
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": ""})
		case strings.Contains(in, "Compaction:") && strings.Contains(in, "always-long"):
			longTries = 0
			result(fmt.Sprintf("3+1|%s", strings.Repeat("y", 700)))
		case strings.Contains(in, "Compaction:") && strings.Contains(in, "make-it-long"):
			failRetry = strings.Contains(in, "then-fail")
			result(strings.Repeat("x", 600))
		case strings.Contains(in, "Compaction:"):
			result(fmt.Sprintf("line with %d images", images))
		case strings.Contains(in, "Go on from where you were"): // after a compaction: the view and the task again
			result(fmt.Sprintf("anchored again: view %v, task %v", strings.Contains(text, "<chat>"), strings.Contains(in, "compact-me")))
		case strings.Contains(in, "compact-me"):
			emit(map[string]any{"type": "system", "subtype": "compact_boundary", "compact_metadata": map[string]any{"trigger": "auto", "pre_tokens": 250000, "post_tokens": 20000}})
		case strings.Contains(in, "ask-then-cancel"):
			// Claude Code asks, then takes the question back.
			emit(map[string]any{"type": "control_request", "request_id": "perm-9", "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "ls"}}})
			time.Sleep(time.Second)
			emit(map[string]any{"type": "control_cancel_request", "request_id": "perm-9"})
			result("taken back")
		case strings.Contains(in, "ask-permission"):
			// As Claude Code in manual mode: it asks, and waits for the
			// answer, taking mode switches meanwhile.
			say(block{Type: "tool_use", ID: "a1", Name: "Bash", Input: []byte(`{"command":"rm x"}`)})
			emit(map[string]any{"type": "control_request", "request_id": "perm-1", "request": map[string]any{
				"subtype": "can_use_tool", "tool_name": "Bash", "display_name": "Bash", "input": map[string]any{"command": "rm x"},
				"description": "Remove x", "tool_use_id": "a1", "permission_suggestions": []map[string]any{
					{"type": "addRules", "rules": []map[string]any{{"toolName": "Bash", "ruleContent": "rm *"}}, "behavior": "allow", "destination": "localSettings"},
					{"type": "addDirectories", "directories": []string{"/far"}, "destination": "session"}}}})
			var modes []string
			for l := range lines {
				var r struct {
					Type     string         `json:"type"`
					Request  map[string]any `json:"request"`
					Response struct {
						RequestID string         `json:"request_id"`
						Response  jsontext.Value `json:"response"`
					} `json:"response"`
				}
				json.Unmarshal([]byte(l), &r)
				if r.Type == "control_request" && r.Request["subtype"] == "set_permission_mode" {
					modes = append(modes, r.Request["mode"].(string))
				}
				if r.Type == "control_response" && r.Response.RequestID == "perm-1" {
					result(fmt.Sprintf("modes %v; %s", modes, r.Response.Response))
					break
				}
				control(l)
			}
		case strings.Contains(in, "wait-for-message"):
			say(textBlock("waiting"), block{Type: "tool_use", ID: "w1", Name: "Bash", Input: []byte(`{"command":"wait"}`)})
			// as Claude Code ends the message while the tool runs
			stream(map[string]any{"type": "content_block_stop"})
			stream(map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 7}})
			select {
			case l := <-lines:
				next, _ := read(l)
				result("got: " + next)
			case <-stop: // as Claude Code, stopped mid-tool
				os.Exit(143)
			}
		case strings.Contains(in, "tool-after-pause"):
			say(textBlock("pausing"))
			time.Sleep(400 * time.Millisecond)
			say(block{Type: "tool_use", ID: "p1", Name: "Bash", Input: []byte(`{"command":"x"}`)})
			next, _ := read(<-lines)
			result("got: " + next)
		case strings.Contains(in, "wait-for-stop"):
			say(textBlock("working"))
			<-stop
			os.Exit(143)
		case strings.Contains(in, "stream-reply"):
			stream(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"output_tokens": 5}}})
			stream(map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "thinking"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "thinking_delta", "thinking": "hmm"}})
			stream(map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "text"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Hello"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": " world"}})
			say(textBlock("Hello world"))
			stream(map[string]any{"type": "message_delta", "usage": map[string]any{"output_tokens": 42}})
			emit(map[string]any{"type": "result", "result": "Hello world"})
		case strings.Contains(in, "stream-tool"):
			stream(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"output_tokens": 1}}})
			stream(map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "name": "Bash"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"command": "echo `}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "input_json_delta", "partial_json": `hi"}`}})
			time.Sleep(500 * time.Millisecond)
			say(block{Type: "tool_use", ID: "s1", Name: "Bash", Input: []byte(`{"command":"echo hi"}`)})
			emit(map[string]any{"type": "user", "message": map[string]any{"content": []block{{Type: "tool_result", ToolID: "s1", Content: []byte(`"hi"`)}}}})
			result("done")
		case strings.Contains(in, "stream-then-stop"):
			stream(map[string]any{"type": "message_start", "message": map[string]any{"usage": map[string]any{"output_tokens": 1}}})
			stream(map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "text"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "half a"}})
			stream(map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": " reply"}})
			<-stop
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": ""})
			os.Exit(143)
		default:
			say(block{Type: "tool_use", Name: "Read", Input: []byte(`{"file_path":"x.png"}`)}, block{Type: "tool_use", Name: "mcp__opt__date", Input: []byte(`{"id":0}`)})
			img, _ := json.Marshal([]block{{Type: "image", Source: &source{Type: "base64", MediaType: "image/png", Data: "iVBORw0KGgoAAAANSUhEUg=="}}})
			emit(map[string]any{"type": "user", "message": map[string]any{"content": []block{{Type: "tool_result", Content: img}}}})
			result(fmt.Sprintf("answer to %q with %d images", text, images))
		}
	}
}

// fakeArgsFile is where each fake agent's calls record their arguments.
var fakeArgsFile = map[*Agent]string{}

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
	fakeArgsFile[a] = argsFile
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
	want := []string{"user: hi 1", `tool: Read {"file_path":"x.png"} 0`, `tool: date {"id":0} 0`, "echo: [image] 1", fmt.Sprintf("opt: answer to %q with 1 images 0", rest+"hi")}
	if !slices.Equal(got, want) {
		t.Fatalf("logged:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	args := calls()[0]
	if flagOf(args, "--model") != "big" || flagOf(args, "--permission-mode") != "auto" || flagOf(args, "--mcp-config") != "turn-mcp" ||
		flagOf(args, "--tools") != "Read" || flagOf(args, "--system-prompt") != "system" || flagOf(args, "--settings") != `{"autoCompactEnabled":false}` {
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
	got, err = a.Compact(context.Background(), nil, append([]block{textBlock("Compaction: x")}, c.fileBlocks([]string{img})...))
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
	// Five tries, the shortest line kept, without a head it echoed.
	got, err = a.Compact(context.Background(), nil, []block{textBlock("Compaction: always-long")})
	if err != nil || got != strings.Repeat("y", 696) {
		t.Fatalf("Compact = %d bytes %.8q, %v; want the fifth try's 696", len(got), got, err)
	}
	if n, u := a.takeCompactions(); n != 4 || u.Read == 0 {
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
		working := len(c.msgs) == 3
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
	if !slices.Equal(got, []string{"user: wait-for-message", "opt: waiting", `tool: Bash {"command":"wait"}`, "user: banana", "opt: got: banana"}) {
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
	if got := a.Spawn("Scout", "look around", "", "", 0); !strings.HasPrefix(got, "Started [Scout]") {
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
	if got, want := report(), fmt.Sprintf("[Scout] answer to %q with 0 images\n\nRan on mid. Full chat: zoom(\"Scout\")", "<chat>\n</chat>You are subagent [Scout]. Your task:\nlook around"); got != want {
		t.Fatalf("report %q, want %q", got, want)
	}
	if !strings.Contains(c.ZoomAgent("[Scout]"), `tool: Read {"file_path":"x.png"}`+"\n\ntool: date {\"id\":0}") {
		t.Fatalf("Scout's log: %q", c.ZoomAgent("Scout"))
	}
	if got := a.Spawn("Scout", "again", "max", "fable", 0); !strings.HasPrefix(got, "Started [Scout-2]") || !strings.HasPrefix(report(), "[Scout-2] ") {
		t.Fatalf("a second Scout: %q", got)
	}
	if args := calls()[0]; flagOf(args, "--model") != "mid" || flagOf(args, "--mcp-config") != "agent-mcp" || flagOf(args, "--tools") != "Read" ||
		flagOf(args, "--settings") != `{"autoCompactEnabled":false}` || slices.Contains(args, "--autocompact") || slices.Contains(args, "--effort") {
		t.Fatalf("subagent args: %q", args)
	}
	if args := calls()[1]; flagOf(args, "--effort") != "max" || flagOf(args, "--model") != "fable" {
		t.Fatalf("a subagent spawned on fable at max effort: %q", args)
	}
	if got := a.Spawn("Scout", "x", "", "two words", 0); !strings.HasPrefix(got, "model is one word") {
		t.Fatal(got)
	}
	if got := a.Spawn("Scout", "x", "huge", "", 0); !strings.HasPrefix(got, "effort must be one of") {
		t.Fatal(got)
	}

	a.Spawn("Sleeper", "wait-for-stop", "", "", 0)
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
	if last := c.msgs[len(c.msgs)-1]; last.Text != "[Sleeper] stopped: opt-chat quit before this subagent finished.\n\nRan on mid. Full chat: zoom(\"Sleeper\")" {
		t.Fatalf("last message: %q", last.Text)
	}
	if !strings.Contains(c.ZoomAgent("Sleeper"), "opt: hurry") {
		t.Fatalf("Sleeper's log: %q", c.ZoomAgent("Sleeper"))
	}
	if got := a.Spawn("Late", "x", "", "", 0); got != "opt-chat is quitting." {
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
		if n == 3 { // its "waiting", and its tool call
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
	if !slices.Equal(got, []string{"user: wait-for-message", "opt: waiting", `tool: Bash {"command":"wait"}`, "user: banana", "opt: got: banana", "user: after quit"}) {
		t.Fatalf("logged: %q", got)
	}
}

// Subagents compact only as the user set it, or as spawn asks, and after a
// compaction get the view and their task again; turns never compact,
// whatever is set.
func TestAgentAutocompact(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	reported := make(chan string, 3)
	a.report = func(text string) { reported <- text }
	report := func() string {
		select {
		case got := <-reported:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	if out, _, err := command(a, "/autocompact 250k"); err != nil || !strings.Contains(out, "at 250k tokens") {
		t.Fatal(out, err)
	}
	if s := loadSettings(c.dir); s.AgentAutocompact != 250_000 {
		t.Fatalf("saved: %d", s.AgentAutocompact)
	}
	for _, bad := range []string{"50k", "2m", "lots"} {
		if _, _, err := command(a, "/autocompact "+bad); err == nil {
			t.Fatalf("/autocompact %s taken", bad)
		}
	}
	a.Spawn("Long", "compact-me", "", "", 0)
	if got := report(); !strings.Contains(got, "anchored again: view true, task true") {
		t.Fatalf("after a compaction: %q", got)
	}
	if !strings.Contains(c.ZoomAgent("Long"), "compacted: 250000 tokens to 20000") {
		t.Fatalf("Long's log: %q", c.ZoomAgent("Long"))
	}
	if got := a.Spawn("Big", "x", "", "", 600_000); !strings.HasPrefix(got, "Started") {
		t.Fatal(got)
	}
	report()
	if got := a.Spawn("Bad", "x", "", "", 5); !strings.HasPrefix(got, "autocompact must be") {
		t.Fatal(got)
	}
	if _, _, err := command(a, "/autocompact off"); err != nil {
		t.Fatal(err)
	}
	a.Spawn("Short", "x", "", "", 0)
	report()
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	all := calls()
	for i, want := range []string{"250000", "600000", "", ""} { // Long, Big, Short, the turn
		args := all[i]
		on := `{"autoCompactEnabled":` + strconv.FormatBool(want != "") + `}`
		if flagOf(args, "--autocompact") != want || flagOf(args, "--settings") != on {
			t.Fatalf("call %d: --autocompact %q, --settings %q; want %q", i, flagOf(args, "--autocompact"), flagOf(args, "--settings"), want)
		}
	}
	a.mu.Lock()
	a.agentWindow = 300_000 // set, and still never a turn's
	a.mu.Unlock()
	a.ownConfig = true
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if args := calls()[4]; slices.Contains(args, "--autocompact") || !strings.Contains(flagOf(args, "--settings"), `"autoCompactEnabled":false`) {
		t.Fatalf("a turn's args: %q", args)
	}
}

// A turn's input names the subagents at work, so Opt never starts one again.
func TestTurnNamesRunning(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	a.running["Scout"], a.running["Ann"] = nil, nil
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	last := c.msgs[len(c.msgs)-1]
	if !strings.Contains(last.Text, `[opt-chat] Subagents at work now: [Ann], [Scout].\n\nhi`) {
		t.Fatalf("reply: %q", last.Text)
	}
	if user := c.msgs[0]; user.Text != "hi" {
		t.Fatalf("logged: %q", user.Text)
	}
}

// A subagent that has reported rests with its call open: a tell sets it to
// work again, with all it knew, and it reports again; after its rest, its
// call ends.
func TestTellResumes(t *testing.T) {
	defer func(d time.Duration) { agentRest = d }(agentRest)
	agentRest = 300 * time.Millisecond
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	reported := make(chan string, 2)
	a.report = func(text string) { reported <- text }
	report := func() string {
		select {
		case got := <-reported:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	a.Spawn("Rest", "first", "", "", 0)
	report()
	if got := a.Tell("Rest", "now the second"); !strings.HasPrefix(got, "Sent: [Rest] is at work on it") {
		t.Fatal(got)
	}
	if got := report(); !strings.HasPrefix(got, `[Rest] answer to "now the second"`) {
		t.Fatalf("second report: %q", got)
	}
	if n := len(calls()); n != 1 {
		t.Fatalf("%d calls: a tell after the report started another", n)
	}
	waitFor(t, "its rest to end", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		_, ok := a.resting["Rest"]
		return !ok
	})
	if got := a.Tell("Rest", "third"); !strings.HasPrefix(got, "No subagent [Rest] is taking messages.") {
		t.Fatal(got)
	}
	if out, _, _ := command(a, "/agents"); out != "No subagent is at work." {
		t.Fatal(out)
	}
}
