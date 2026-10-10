// opt-chat: one chat with Claude that never ends. The chat is the memory: an
// append-only log, compressed into a binary tree of one-line summaries, of
// which every turn sees a constant-size view. See README.md.
package main

import (
	"bufio"
	"cmp"
	"context"
	_ "embed"
	"errors"
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
	"syscall"
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
	importPath := flag.String("import", "", "import past chats, then chat: a ~/.claude or ~/.codex directory, a session's .jsonl, a Claude or ChatGPT export (.zip), or a text file whose lines become notes")
	importSince := flag.String("import-since", "", "import only messages from this day on (YYYY-MM-DD)")
	importTools := flag.Bool("import-tools", false, "import tool calls and results too")
	importSkip := flag.String("import-skip", "", "skip sessions whose directory (or a chat's title) contains any of these, comma-separated")
	repl := flag.Bool("repl", false, "chat in the terminal instead of the browser")
	addr := flag.String("addr", "", "where the web chat listens, as 127.0.0.1:port (empty: the last run's port when free, else any)")
	open := flag.Bool("open", true, "open the web chat in the browser")
	flag.Parse()

	c, err := openChat(*dir)
	if err != nil {
		fatal("%v", err)
	}
	if *importPath != "" {
		o := importOpts{tools: *importTools, skip: strings.Split(*importSkip, ",")}
		if *importSince != "" {
			if o.since, err = time.ParseInLocation(time.DateOnly, *importSince, time.Local); err != nil {
				fatal("-import-since: %v", err)
			}
		}
		switch err := importChats(c, *importPath, o, os.Stdin, os.Stdout); {
		case errors.Is(err, errDeclined):
			fmt.Println("Nothing imported.")
			return
		case err != nil:
			fatal("import: %v", err)
		}
	}
	system := prompt
	if b, err := os.ReadFile(filepath.Join(*dir, "AGENTS.md")); err == nil {
		system += "\n# The user's instructions\n\n" + string(b)
	}
	hb := newHub()
	if *repl {
		hb.print = replPrinter()
	} else {
		hb.print = webPrinter()
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
	c.hub = hb
	c.compact = a.Compact
	if *repl {
		for _, k := range c.view.keys {
			if c.built(k) { // the rest are summarized before the first turn
				fmt.Println(c.line(k))
			}
		}
	}
	backlog := ""
	if n := len(c.ready0); n > 0 {
		backlog = fmt.Sprintf("; %s to summarize", count(n, "message"))
	}
	dim(fmt.Sprintf("[%d messages; the view is %d lines, %d bytes; %s%s]", len(c.msgs), len(c.view.keys), c.view.size(c), *dir, backlog))
	c.pump()
	c.mu.Unlock()
	stopStatus := make(chan struct{})
	go watchStatus(c, a, hb, stopStatus)

	quit := func() {
		c.mu.Lock()
		c.compact = nil // none starts from here on; the next run builds what is left
		c.mu.Unlock()
		h.quit()
		close(stopStatus)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	if !*repl {
		w, err := serveWeb(h, *addr)
		if err != nil {
			fatal("web: %v", err)
		}
		fmt.Printf("Open %s\n", w.url)
		dim("Ctrl-C quits.")
		if *open {
			openBrowser(w.url)
		}
		<-sig
		fmt.Println()
		dim("[quitting: stopping every call...]")
		quit()
		return
	}
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
				h.input(typed(c, text))
			}
		case <-sig:
			back, ok := h.stop()
			if !ok {
				quit()
				return
			}
			dim("[stopped]")
			for _, m := range back {
				dim("[not sent: " + m.text + "]")
			}
		}
	}
}

// typed is a message typed in the terminal, with the files it names.
func typed(c *Chat, text string) message {
	paths := filePaths(text)
	for _, p := range paths {
		if hidden(p) { // as in the page, where it is attached only once asked
			c.hub.notice("info", "[%s is hidden: not attached; Opt can still read it]", p)
		}
	}
	atts, err := c.attachPaths(slices.DeleteFunc(paths, hidden))
	if err != nil {
		c.hub.notice("warn", "%v", err)
	}
	if len(atts) > 0 {
		c.hub.notice("info", "[%s attached]", count(len(atts), "file"))
	}
	return c.compose(text, atts)
}

// harness runs the chat's turns, one at a time, and holds the messages that
// wait for one: the queue, which the pages show and let the user edit. The
// user's message joins the turn underway as soon as the model could read
// it: at once while a tool call runs, else at the next tool call; or it
// starts the next turn. An agent's report joins at once.
type harness struct {
	agent    *Agent
	mu       sync.Mutex
	queue    []queued
	lastID   int
	turn     *Turn
	cancel   context.CancelFunc
	running  bool
	stopped  bool // the turn underway was stopped, and its queue taken
	hurry    bool // sent now: the next turn waits for no summary
	quitting bool
	loops    sync.WaitGroup
}

type queued struct {
	id int
	message
}

func (h *harness) input(m message) {
	// A report joins the turn underway at once, and so does the user's
	// message while a tool call runs: the model gets it with the result.
	// It is sent without h.mu, as everything sent to a call is: a write to
	// the call can wait on the call, which can wait on h.mu (flush).
	h.mu.Lock()
	t := h.turn
	if h.quitting {
		t = nil
	}
	h.mu.Unlock()
	if t != nil && (m.kind != "user" || t.inTool()) && t.Send(&m) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enqueue(m)
}

// inputNow queues m and stops the turn underway, or the wait before it, so
// that m goes at once: send now.
func (h *harness) inputNow(m message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.enqueue(m) {
		return
	}
	h.hurry = true
	if h.cancel != nil {
		h.cancel()
	}
}

// enqueue adds m to the queue and starts the turns' loop if it is idle;
// false when quitting. Caller holds h.mu.
func (h *harness) enqueue(m message) bool {
	if h.quitting { // a report from a subagent being stopped
		logAll(h.agent.chat, []message{m})
		return false
	}
	h.lastID++
	h.queue = append(h.queue, queued{h.lastID, m})
	h.publish()
	if !h.running {
		h.running = true
		h.loops.Add(1)
		go h.loop()
	}
	return true
}

// publish shows the pages the user's messages waiting in the queue.
func (h *harness) publish() {
	var items []queueItem
	for _, q := range h.queue {
		if q.kind == "user" && !q.logged {
			items = append(items, queueItem{ID: q.id, Text: q.text, Files: q.files})
		}
	}
	h.agent.chat.hub.setQueue(items)
}

// flush hands the turn underway what waits in the queue, as a tool call
// starts: it reaches the model with the call's result. One at a time, so
// that a stop meanwhile hands back all but the one on its way.
func (h *harness) flush() {
	for {
		h.mu.Lock()
		t := h.turn
		if t == nil || len(h.queue) == 0 || h.stopped || h.quitting {
			h.mu.Unlock()
			return
		}
		it := h.queue[0]
		h.queue = h.queue[1:]
		h.publish()
		h.mu.Unlock()
		if t.Send(&it.message) {
			continue
		}
		h.mu.Lock()
		if h.stopped || h.quitting { // it was on its way: in the log, unanswered, as stop says
			logAll(h.agent.chat, []message{it.message})
		} else { // the call ended first: it starts the next turn
			h.queue = append([]queued{it}, h.queue...)
			h.publish()
		}
		h.mu.Unlock()
		return
	}
}

// edit changes a queued message's text; false once it is sent.
func (h *harness) edit(id int, text string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.queue, func(q queued) bool { return q.id == id && !q.logged })
	if i < 0 {
		return false
	}
	h.queue[i].text = text
	h.publish()
	return true
}

// pull takes a queued message out, to drop it or edit it in the input;
// false once it is sent.
func (h *harness) pull(id int) (message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := slices.IndexFunc(h.queue, func(q queued) bool { return q.id == id && q.kind == "user" && !q.logged })
	if i < 0 {
		return message{}, false
	}
	m := h.queue[i].message
	h.queue = slices.Delete(h.queue, i, i+1)
	h.publish()
	return m, true
}

// sendNow stops the turn underway, or the wait before it, and starts the
// next one at once with the queue; false when nothing waits.
func (h *harness) sendNow() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running || !slices.ContainsFunc(h.queue, func(q queued) bool { return q.kind == "user" }) {
		return false
	}
	h.hurry = true
	if h.cancel != nil {
		h.cancel()
	}
	return true
}

// stop cancels the turn underway, or the wait before it, and hands back the
// user's queued messages, unsent, as Claude Code puts them back in the
// input; reports waiting stay in the log, unanswered. False when idle.
func (h *harness) stop() ([]message, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.running || h.cancel == nil {
		return nil, false
	}
	h.stopped = true
	h.cancel()
	var back []message
	for _, q := range h.take() {
		if q.kind == "user" && !q.logged {
			back = append(back, q)
		} else {
			logAll(h.agent.chat, []message{q})
		}
	}
	h.publish()
	return back, true
}

// take empties the queue.
func (h *harness) take() []message {
	msgs := make([]message, len(h.queue))
	for i, q := range h.queue {
		msgs[i] = q.message
	}
	h.queue = nil
	return msgs
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

func (h *harness) loop() {
	defer h.loops.Done()
	c := h.agent.chat
	for {
		h.mu.Lock()
		if len(h.queue) == 0 || h.quitting {
			logAll(c, h.take()) // stays in the log, unanswered
			h.running, h.cancel, h.stopped, h.hurry = false, nil, false, false
			h.publish()
			c.hub.setTurn(func(lt *liveTurn) { *lt = liveTurn{} })
			h.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel, h.stopped = cancel, false
		hurry := h.hurry
		h.hurry = false
		h.mu.Unlock()
		c.mu.Lock()
		from := len(c.msgs) // no tool call before it runs now
		c.mu.Unlock()
		c.hub.setTurn(func(lt *liveTurn) { *lt = liveTurn{Phase: "settling", Since: time.Now().UnixMilli(), From: from} })
		if !hurry {
			// Wait until every message is summarized: no call sees a
			// placeholder, unless behind a backlog (see settle).
			c.settle(ctx, func(n, written int) {
				c.hub.setTurn(func(lt *liveTurn) { lt.Backlog, lt.Written = n, written })
			})
		}
		h.mu.Lock()
		if h.stopped || h.quitting { // stop took the queue
			h.mu.Unlock()
			cancel()
			continue
		}
		// Sent now while it waited, or as the wait ended: on at once, with a
		// call of its own (the canceled ctx was the wait's).
		if ctx.Err() != nil {
			cancel()
			ctx, cancel = context.WithCancel(context.Background())
			h.cancel = cancel
		}
		h.hurry = false
		msgs := h.take()
		h.publish()
		if len(msgs) == 0 { // all pulled back while it waited
			h.mu.Unlock()
			cancel()
			continue
		}
		t := &Turn{agent: h.agent, onTool: h.flush}
		h.turn = t
		h.mu.Unlock()
		if err := t.Run(ctx, msgs); err != nil && ctx.Err() == nil {
			c.hub.notice("warn", "turn: %v", err)
		}
		h.mu.Lock()
		h.turn = nil
		var left []queued
		for _, m := range t.detach() { // taken, and logged, but the call ended before it got them
			h.lastID++
			left = append(left, queued{h.lastID, m})
		}
		if !h.stopped { // stopped: they stay in the log, unanswered, as stop says
			h.queue = append(left, h.queue...)
		}
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
			c.Log(m.kind, m.full(), m.images...)
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

// replPrinter shows the chat in the terminal, for -repl.
func replPrinter() func(any) {
	var told time.Time
	return func(v any) {
		switch u := v.(type) {
		case msgsUpdate:
			for _, m := range u.Msgs {
				switch m.Kind {
				case "opt", "work":
					fmt.Println(m.Text)
				case "tool":
					dim("→ " + oneLine(m.Text, 200))
				case "echo":
					dim("  " + oneLine(m.Text, 160))
				}
			}
		case thoughtUpdate:
			dim(u.S)
		case noticeUpdate:
			if u.Level == "warn" {
				warn("%s", u.S)
			} else {
				dim(u.S)
			}
		case endUpdate:
			dim(u.End.String())
		case turnUpdate:
			// Merges go first, so after an import the count stays put for a long while.
			if t := u.Turn; t.Phase == "settling" && t.Backlog > 0 && time.Since(told) >= time.Minute {
				s := "[summarizing " + count(t.Backlog, "message") + " first"
				if t.Written > 0 {
					s += "; " + count(t.Written, "line") + " written so far"
				}
				dim(s + "...]")
				told = time.Now()
			} else if t.Phase == "reading" {
				told = time.Time{}
			}
		}
	}
}

// webPrinter shows in the terminal, while the chat is in the browser, only
// warnings and how the summaries come along.
func webPrinter() func(any) {
	var told time.Time
	return func(v any) {
		switch u := v.(type) {
		case noticeUpdate:
			if u.Level == "warn" {
				warn("%s", u.S)
			}
		case statusUpdate:
			s := u.Status
			switch left := s.Backlog + s.Merges; {
			case left > 0 && time.Since(told) >= time.Minute:
				var todo []string
				if s.Backlog > 0 {
					todo = append(todo, count(s.Backlog, "message"))
				}
				if s.Merges > 0 {
					todo = append(todo, count(s.Merges, "merge"))
				}
				dim(fmt.Sprintf("[summarizing: %s left; %s written]", strings.Join(todo, " and "), count(s.Written, "line")))
				told = time.Now()
			case left == 0 && !told.IsZero():
				dim("[every message is summarized]")
				told = time.Time{}
			}
		}
	}
}

func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, "\x1b[33m"+f+"\x1b[0m\n", a...) }

func fatal(f string, a ...any) {
	warn(f, a...)
	stopChildren()
	os.Exit(1)
}
