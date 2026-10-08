// opt-chat: one chat with Claude that never ends. The chat is the memory: an
// append-only log, compressed into a binary tree of one-line summaries, of
// which every turn sees a constant-size view. See README.md.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
	model := flag.String("model", "opus", "model for turns")
	effort := flag.String("effort", "", "effort for turns: low, medium, high, xhigh, max (empty: the model's default)")
	cmodel := flag.String("compact-model", "haiku", "model for compactions")
	ceffort := flag.String("compact-effort", "xhigh", "effort for compactions")
	perm := flag.String("permission-mode", "auto", "Claude Code permission mode for turns")
	tools := flag.String("tools", "Bash,Read,Edit,Write,WebSearch,WebFetch", "Claude Code tools for turns")
	flag.Parse()

	c, err := openChat(*dir)
	if err != nil {
		fatal("%v", err)
	}
	system := prompt
	if b, err := os.ReadFile(filepath.Join(*dir, "AGENTS.md")); err == nil {
		system += "\n# The user's instructions\n\n" + string(b)
	}
	h := &harness{}
	a := &Agent{chat: c, system: system, model: *model, effort: *effort,
		cmodel: *cmodel, ceffort: *ceffort, perm: *perm, cwd: *cwd, tools: *tools,
		running: map[string]*proc{}, report: func(text string) { h.input(message{"work", text}) }}
	h.agent = a
	if a.mcpConfig, err = serveMCP(a); err != nil {
		fatal("mcp: %v", err)
	}

	c.mu.Lock()
	c.compact = a.Compact
	lines := c.lines(c.view.keys)
	fmt.Println(strings.Join(lines, "\n"))
	dim(fmt.Sprintf("[%d messages; the view above is %d lines, %d bytes; %s]", len(c.msgs), len(lines), c.view.size(c), *dir))
	c.pump()
	c.mu.Unlock()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	in := readInput(os.Stdin)
	for {
		select {
		case text, ok := <-in:
			if !ok {
				h.stop()
				return
			}
			if strings.TrimSpace(text) != "" {
				h.input(message{"user", text})
			}
		case <-sig:
			if !h.stop() {
				return
			}
			dim("[stopped]")
		}
	}
}

// harness queues the user's messages: one turn at a time, and a message sent
// while a turn runs reaches it between tool calls.
type harness struct {
	agent   *Agent
	mu      sync.Mutex
	queue   []message
	turn    *Turn
	cancel  context.CancelFunc
	running bool
}

func (h *harness) input(m message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.turn != nil && h.turn.Send(m) {
		return
	}
	h.queue = append(h.queue, m)
	if !h.running {
		h.running = true
		go h.loop()
	}
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
	c := h.agent.chat
	for {
		ctx, cancel := context.WithCancel(context.Background())
		h.mu.Lock()
		if len(h.queue) == 0 {
			h.running, h.cancel = false, nil
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
			for _, m := range msgs {
				c.Log(m.kind, m.text) // stays in the log, unanswered
			}
		} else if err := t.Run(ctx, msgs); err != nil && ctx.Err() == nil {
			warn("turn: %v", err)
		}
		h.mu.Lock()
		h.turn = nil
		h.mu.Unlock()
		cancel()
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

func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, "\x1b[33m"+f+"\x1b[0m\n", a...) }

func fatal(f string, a ...any) {
	warn(f, a...)
	os.Exit(1)
}
