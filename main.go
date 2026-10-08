// opt-chat: one chat with Claude that never ends. The chat is the memory: an
// append-only log, compressed into a binary tree of one-line summaries, of
// which every turn sees a constant-size view. See README.md.
package main

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed prompt.md
var prompt string

func main() {
	home, _ := os.UserHomeDir()
	dir := flag.String("dir", filepath.Join(home, ".opt-chat"), "where the chat lives; its AGENTS.md holds your instructions")
	cwd := flag.String("cwd", home, "where Opt's tools run")
	model := flag.String("model", "fable", "model for turns")
	effort := flag.String("effort", "", "effort for turns: low, medium, high, xhigh, max (empty: the model's default)")
	amodel := flag.String("agent-model", "opus", "model for subagents")
	aeffort := flag.String("agent-effort", "", "effort for subagents (empty: the model's default)")
	cmodel := flag.String("compact-model", "haiku", "model for compactions")
	ceffort := flag.String("compact-effort", "xhigh", "effort for compactions")
	perm := flag.String("permission-mode", "auto", "Claude Code permission mode")
	tools := flag.String("tools", "Bash,Read,Edit,Write,WebSearch,WebFetch", "Claude Code tools")
	importFile := flag.String("import", "", "log each line of this file as a note (memories from before the chat), then exit")
	flag.Parse()

	c, err := openChat(*dir)
	if err != nil {
		fatal("%v", err)
	}
	if *importFile != "" {
		b, err := os.ReadFile(*importFile)
		if err != nil {
			fatal("%v", err)
		}
		n := 0
		for line := range strings.Lines(string(b)) {
			if line = strings.TrimSpace(line); line != "" {
				c.Log("note", line)
				n++
			}
		}
		fmt.Printf("imported %d notes; the next run summarizes them\n", n)
		return
	}
	system := prompt
	if b, err := os.ReadFile(filepath.Join(*dir, "AGENTS.md")); err == nil {
		system += "\n# The user's instructions\n\n" + string(b)
	}
	h := &harness{}
	a := &Agent{chat: c, system: system, model: *model, effort: *effort,
		amodel: *amodel, aeffort: *aeffort, cmodel: *cmodel, ceffort: *ceffort, perm: *perm, cwd: *cwd, tools: *tools,
		running: map[string]*proc{}, writing: map[string]chan struct{}{}, report: func(text string) { h.input(message{kind: "work", text: text}) }}
	a.life, a.stop = context.WithCancel(context.Background())
	h.agent = a
	if a.mcpConfig, a.agentMCP, err = serveMCP(a); err != nil {
		fatal("mcp: %v", err)
	}

	c.mu.Lock()
	c.compact = a.Compact
	lines := c.lines(c.view.keys)
	fmt.Println(strings.Join(lines, "\n"))
	dim(fmt.Sprintf("[%d messages; the view above is %d lines, %d bytes; %s]", len(c.msgs), len(lines), c.view.size(c), *dir))
	c.pump()
	c.mu.Unlock()

	quit := func() {
		c.mu.Lock()
		c.compact = nil // none starts from here on; the next run builds what is left
		c.mu.Unlock()
		h.quit()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	in := readInput(os.Stdin)
	for {
		select {
		case text, ok := <-in:
			if !ok {
				quit()
				return
			}
			if out, ok := command(a, text); ok {
				dim(out)
			} else if strings.TrimSpace(text) != "" {
				h.input(message{kind: "user", text: text, images: attach(c, text)})
			}
		case <-sig:
			if !h.stop() {
				quit()
				return
			}
			dim("[stopped]")
		}
	}
}

// harness queues the user's messages: one turn at a time, and a message sent
// while a turn runs reaches it between tool calls.
type harness struct {
	agent    *Agent
	mu       sync.Mutex
	queue    []message
	turn     *Turn
	cancel   context.CancelFunc
	running  bool
	quitting bool
	loops    sync.WaitGroup
}

func (h *harness) input(m message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.quitting { // a report from a subagent being stopped
		h.agent.chat.Log(m.kind, m.text, m.images...)
		return
	}
	if h.turn != nil && h.turn.Send(&m) {
		return
	}
	h.queue = append(h.queue, m)
	if !h.running {
		h.running = true
		h.loops.Add(1)
		go h.loop()
	}
}

// quit stops every call and subagent underway, and returns once they have
// ended and the messages waiting are logged, unanswered.
func (h *harness) quit() {
	h.mu.Lock()
	h.quitting = true
	if h.cancel != nil {
		h.cancel()
	}
	h.mu.Unlock()
	h.agent.quit()
	h.loops.Wait()
}

// stop cancels the wait or the turn underway; false when idle.
func (h *harness) stop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel == nil {
		return false
	}
	h.cancel()
	return true
}

func (h *harness) loop() {
	defer h.loops.Done()
	c := h.agent.chat
	for {
		ctx, cancel := context.WithCancel(context.Background())
		h.mu.Lock()
		if len(h.queue) == 0 || h.quitting {
			logAll(c, h.queue) // stays in the log, unanswered
			h.queue, h.running, h.cancel = nil, false, nil
			h.mu.Unlock()
			cancel()
			return
		}
		h.cancel = cancel
		h.mu.Unlock()
		// Wait until every message is summarized: no call ever sees a placeholder.
		err := c.settle(ctx, func(n int) { dim(fmt.Sprintf("[summarizing %d messages...]", n)) })
		h.mu.Lock()
		msgs := h.queue
		h.queue = nil
		t := &Turn{agent: h.agent}
		if err == nil {
			h.turn = t
		}
		h.mu.Unlock()
		if err != nil {
			logAll(c, msgs) // stays in the log, unanswered
		} else if err := t.Run(ctx, msgs); err != nil && ctx.Err() == nil {
			warn("turn: %v", err)
		}
		h.mu.Lock()
		h.turn = nil
		h.queue = append(t.detach(), h.queue...) // taken, but the call ended before it got them
		h.mu.Unlock()
		cancel()
	}
}

var (
	commandWord = regexp.MustCompile(`(?s)^\s*/([A-Za-z]+)(?:\s+(.*?))?\s*$`) // "/word args", never a path like /Users/me/shot.png
	modelName   = regexp.MustCompile(`^[\w.:\[\]-]+$`)
)

// command runs a line that starts with /word, and says what it did; false
// for a message. A /word that is no command is not sent either, as in
// UniiChat: a typo never reaches the chat for good.
func command(a *Agent, line string) (string, bool) {
	m := commandWord.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch word, arg := strings.ToLower(m[1]), m[2]; {
	case word == "model" && modelName.MatchString(arg):
		a.model = arg
	case word == "model" && arg != "":
		return "A model is one word, as for -model: fable, opus, sonnet, or a full name.", true
	case word == "effort" && arg == "default":
		a.effort = ""
	case word == "effort" && slices.Contains(efforts, arg):
		a.effort = arg
	case word == "effort" && arg != "":
		return "effort must be one of " + strings.Join(efforts, ", ") + ", or default.", true
	case word == "model" || word == "effort":
	case word == "agents":
		names := slices.Sorted(maps.Keys(a.running))
		if len(names) == 0 {
			return "No subagent is at work.", true
		}
		return "At work: [" + strings.Join(names, "], [") + "]. Each one's steps: " + filepath.Join(a.chat.dir, "agents", "<Name>.log"), true
	case word == "help":
		return "/model [name]       show or set the model for turns, as -model (until quit)\n" +
			"/effort [level]     show or set their effort: " + strings.Join(efforts, ", ") + ", or default\n" +
			"/agents             list the subagents at work\n" +
			"Ctrl-C stops the turn; Ctrl-C when idle, or Ctrl-D, quits.", true
	default:
		return "/" + m[1] + " is no command; nothing was sent. /help lists the commands.", true
	}
	return fmt.Sprintf("Turns: %s, effort %s.", a.model, cmp.Or(a.effort, "default")), true
}

func logAll(c *Chat, msgs []message) {
	for _, m := range msgs {
		if !m.logged {
			c.Log(m.kind, m.text, m.images...)
		}
	}
}

// readInput yields the user's messages, one per line, keeping a paste (many
// lines arriving at once) as one message.
func readInput(r io.Reader) <-chan string {
	raw := make(chan string)
	go func() {
		s := bufio.NewScanner(r)
		s.Buffer(nil, 64<<20)
		for s.Scan() {
			raw <- s.Text()
		}
		close(raw)
	}()
	out := make(chan string)
	go func() {
		defer close(out)
		for line := range raw {
			lines := []string{line}
		paste:
			for {
				select {
				case l, ok := <-raw:
					if !ok {
						break paste
					}
					lines = append(lines, l)
				case <-time.After(50 * time.Millisecond):
					break paste
				}
			}
			out <- strings.Join(lines, "\n")
		}
	}()
	return out
}

var (
	pathToken = regexp.MustCompile(`'[^']+'|"[^"]+"|(?:\\.|[^\s'"\\])+`) // as a terminal drops a path: quoted, or with escaped spaces
	escaped   = regexp.MustCompile(`\\(.)`)
)

// imagePaths finds the image files a message names.
func imagePaths(text string) []string {
	var out []string
	for _, tok := range pathToken.FindAllString(text, -1) {
		p := strings.TrimRight(escaped.ReplaceAllString(tok, "$1"), ".,;:!?)") // a path ending a sentence
		if tok[0] == '\'' || tok[0] == '"' {
			p = tok[1 : len(tok)-1]
		}
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			home, _ := os.UserHomeDir()
			p = filepath.Join(home, rest)
		}
		if !slices.Contains([]string{".png", ".jpg", ".jpeg", ".gif", ".webp"}, strings.ToLower(filepath.Ext(p))) {
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}

// attach saves the images a message names, for it to carry.
func attach(c *Chat, text string) []string {
	var names []string
	for _, p := range imagePaths(text) {
		b, err := os.ReadFile(p)
		if err == nil {
			var name string
			if name, err = c.saveImage(b); err == nil && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
		if err != nil {
			warn("%s: %v", p, err)
		}
	}
	if len(names) > 0 {
		dim(fmt.Sprintf("[%d image(s) attached]", len(names)))
	}
	return names
}

func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, "\x1b[33m"+f+"\x1b[0m\n", a...) }

func fatal(f string, a ...any) {
	warn(f, a...)
	os.Exit(1)
}
