package main

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Agent runs `claude -p` for turns and compactions: Claude Code is the tool
// loop (Bash, Read, Edit, ...); this program only owns the memory.
type Agent struct {
	chat            *Chat
	system          string // byte-identical across calls: the head of every cached prefix
	mcpConfig       string
	model, effort   string
	cmodel, ceffort string
	perm, cwd       string
	tools           string
}

type block struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Thinking string         `json:"thinking,omitempty"`
	Name     string         `json:"name,omitempty"`
	Input    jsontext.Value `json:"input,omitzero"`
	Content  jsontext.Value `json:"content,omitzero"` // tool_result: a string or blocks
	IsError  bool           `json:"is_error,omitzero"`
}

type usage struct {
	Input  int `json:"input_tokens"`
	Write  int `json:"cache_creation_input_tokens"`
	Read   int `json:"cache_read_input_tokens"`
	Output int `json:"output_tokens"`
}

func (u *usage) add(v usage) {
	u.Input += v.Input
	u.Write += v.Write
	u.Read += v.Read
	u.Output += v.Output
}

func (u usage) String() string {
	k := func(n int) string { return fmt.Sprintf("%.1fk", float64(n)/1000) }
	return fmt.Sprintf("cache read %s, write %s, in %s, out %s", k(u.Read), k(u.Write), k(u.Input), k(u.Output))
}

type event struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	Parent  *string `json:"parent_tool_use_id"`
	Message struct {
		Content jsontext.Value `json:"content"`
	} `json:"message"`
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Usage   usage  `json:"usage"`
}

func (e event) blocks() []block {
	var bs []block
	if err := json.Unmarshal(e.Message.Content, &bs); err != nil {
		var s string
		if json.Unmarshal(e.Message.Content, &s) == nil {
			bs = []block{{Type: "text", Text: s}}
		}
	}
	return bs
}

// proc is one `claude -p` process speaking stream-json both ways.
type proc struct {
	cmd    *exec.Cmd
	mu     sync.Mutex
	stdin  io.WriteCloser // nil once closed
	out    *bufio.Scanner
	stderr strings.Builder
}

func (a *Agent) start(ctx context.Context, model, effort string, turn bool) (*proc, error) {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", model, "--system-prompt", a.system,
		// Nothing of the user's Claude Code setup leaks in: no hooks, plugins, CLAUDE.md or MCP servers.
		"--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence"}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if turn {
		args = append(args, "--tools", a.tools, "--mcp-config", a.mcpConfig,
			"--allowedTools", "mcp__opt__zoom,mcp__opt__date", "--permission-mode", a.perm)
	} else {
		// A compaction calls no tools. It runs on another model, so it could
		// not read the turns' cache entries anyway.
		args = append(args, "--tools", "", "--mcp-config", `{"mcpServers":{}}`)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = a.cwd
	p := &proc{cmd: cmd}
	cmd.Stderr = &p.stderr
	var err error
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p.out = bufio.NewScanner(stdout)
	p.out.Buffer(nil, 64<<20)
	return p, cmd.Start()
}

// send writes one user message; false once stdin is closed.
func (p *proc) send(texts ...string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return false
	}
	content := make([]block, 0, len(texts))
	for _, t := range texts {
		if t != "" {
			content = append(content, block{Type: "text", Text: t})
		}
	}
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	_, err := p.stdin.Write(append(b, '\n'))
	return err == nil
}

func (p *proc) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		p.stdin.Close()
		p.stdin = nil
	}
}

func (p *proc) next() (event, bool) {
	for p.out.Scan() {
		var e event
		if json.Unmarshal(p.out.Bytes(), &e) == nil {
			return e, true
		}
	}
	return event{}, false
}

func (p *proc) wait() error {
	p.close()
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("%w: %s", err, lastLine(p.stderr.String()))
	}
	return nil
}

// viewMessages splits rendered view lines in two. The first message holds
// whole blocks of 4 lines and nothing else, so Claude Code's cache mark at
// its end falls right after the view: the next call, whose view only grew,
// finds that entry within the 20 blocks the API looks back. The model
// answers it with "ok". The second message is the remaining lines, then
// the input itself. nil first message: too few lines to bother.
func viewMessages(lines []string, input string) (first []string, second []string) {
	whole := len(lines) - len(lines)%blockLen
	for j := 0; j < whole; j += blockLen {
		first = append(first, strings.Join(lines[j:j+blockLen], "\n")+"\n")
	}
	tail := strings.Join(lines[whole:], "\n")
	if tail != "" {
		tail += "\n"
	}
	if first == nil {
		tail = "<chat>\n" + tail
	} else {
		first[0] = "<chat>\n" + first[0]
	}
	return first, []string{tail + "</chat>", input}
}

// ack sends the view's first message and waits out the model's "ok".
func ack(p *proc, first []string, u *usage) error {
	if first == nil {
		return nil
	}
	p.send(first...)
	for {
		e, ok := p.next()
		if !ok {
			return errors.New("claude exited during the view ack")
		}
		if e.Type == "result" {
			u.add(e.Usage)
			if e.IsError {
				return fmt.Errorf("view ack: %s", e.Result)
			}
			return nil
		}
	}
}

// Compact builds one tree line, enforcing the size with the ruler, the cut, and retries.
func (a *Agent) Compact(ctx context.Context, lines []string, task string) (string, error) {
	p, err := a.start(ctx, a.cmodel, a.ceffort, false)
	if err != nil {
		return "", err
	}
	defer p.wait()
	var u usage
	first, second := viewMessages(lines, task)
	if err := ack(p, first, &u); err != nil {
		return "", err
	}
	p.send(second...)
	best := ""
	for try := 1; ; try++ {
		e, ok := p.next()
		if !ok {
			return "", fmt.Errorf("claude exited: %s", lastLine(p.stderr.String()))
		}
		if e.Type != "result" {
			continue
		}
		if e.IsError {
			return "", fmt.Errorf("compaction: %s", e.Result)
		}
		line := strings.TrimSpace(e.Result)
		if line == "" {
			return "", errors.New("empty compaction")
		}
		if best == "" || len(line) < len(best) {
			best = line
		}
		// A few bytes over is fine: the view measures real sizes.
		if len(line) <= nodeMax || try >= 5 {
			return best, nil
		}
		p.send(fmt.Sprintf("Too long: your line is %d bytes, over the 512-byte limit. Write\n"+
			"the whole line again for the same <input>, cutting just enough of the\n"+
			"least valuable items to fit before this cut:\n%s| ← LIMIT", len(line), splitText(line, nodeMax)[0]))
	}
}

// Turn is one fresh `claude -p` call: [system] [view] [message], logging
// each reply, tool call and result as it happens.
type Turn struct {
	agent *Agent
	mu    sync.Mutex
	p     *proc // set once the message is sent; messages can join until it closes
}

// Send hands a message to the running call, between its tool calls.
func (t *Turn) Send(text string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.p == nil {
		return false
	}
	t.agent.chat.Log("user", text) // logged before it is sent: the log never misses what the model saw
	return t.p.send(text)
}

func (t *Turn) Run(ctx context.Context, texts []string) error {
	a, c := t.agent, t.agent.chat
	c.mu.Lock()
	lines := c.lines(c.view.keys) // rendered before the new message is logged
	c.mu.Unlock()
	msg := strings.Join(texts, "\n\n")
	for _, s := range texts {
		c.Log("user", s)
	}
	p, err := a.start(ctx, a.model, a.effort, true)
	if err != nil {
		return err
	}
	var u usage
	first, second := viewMessages(lines, msg)
	if err := ack(p, first, &u); err != nil {
		p.wait()
		return err
	}
	t.mu.Lock()
	p.send(second...)
	t.p = p
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.p = nil
		t.mu.Unlock()
	}()
	for {
		e, ok := p.next()
		if !ok {
			break
		}
		if e.Parent != nil {
			continue // a subagent's own steps stay out of the chat; its report comes back as a tool result
		}
		switch e.Type {
		case "assistant":
			for _, b := range e.blocks() {
				switch b.Type {
				case "thinking":
					if b.Thinking != "" {
						dim(b.Thinking)
					}
				case "text":
					fmt.Println(b.Text)
					c.Log("opt", b.Text)
				case "tool_use":
					call := b.Name + " " + string(b.Input)
					dim("→ " + oneLine(call, 200))
					c.Log("tool", call)
				}
			}
		case "user":
			for _, b := range e.blocks() {
				if b.Type == "tool_result" {
					out := resultText(b.Content)
					dim("  " + oneLine(out, 160))
					c.Log("echo", clip(out, clipMax))
				}
			}
		case "result":
			u.add(e.Usage)
			if e.IsError {
				warn("turn: %s", e.Result)
			}
			// Done after this result: messages sent later start a new turn.
			// One sent before is already queued in Claude Code, which runs it before exiting.
			t.mu.Lock()
			p.close()
			t.mu.Unlock()
		}
	}
	err = p.wait()
	dim("[" + u.String() + "]")
	return err
}

func resultText(v jsontext.Value) string {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var bs []block
	json.Unmarshal(v, &bs)
	var parts []string
	for _, b := range bs {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = splitText(s, n)[0] + "…"
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if _, after, ok := strings.CutLast(s, "\n"); ok {
		return after
	}
	return s
}

func dim(s string) { fmt.Fprintf(os.Stdout, "\x1b[2m%s\x1b[0m\n", s) }
