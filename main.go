// opt-chat: one chat with Claude that never ends. The chat is the memory: an
// append-only log, compressed into a binary tree of one-line summaries, of
// which every turn sees a constant-size view. See README.md.
package main

import (
	"bufio"
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
	"strconv"
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
	model := flag.String("model", "fable", "model for turns (unset: the one the page's picker saved, if any)")
	effort := flag.String("effort", "high", "effort for turns: low, medium, high, xhigh, max (unset: the picker's, if saved)")
	amodel := flag.String("agent-model", "opus", "model for subagents")
	aeffort := flag.String("agent-effort", "high", "effort for subagents")
	agentCap := flag.Int("agent-compact-max", 400_000, fmt.Sprintf("compact a subagent's context once it passes this many tokens, between two steps (%dk to %dk; 0: never). The chat's own never compacts", windowMin/1000, windowMax/1000))
	cmodel := flag.String("compact-model", "sonnet", "model for compactions (the recipe's: haiku)")
	ceffort := flag.String("compact-effort", "medium", "effort for compactions")
	cbare := flag.Bool("compact-bare", true, "memory summary calls (compactions) on a different model from the turns get no tools: they share no cache with turns, and Claude Code adds tool reminders, such as a commit attribution naming the summary call's model, that summaries took for the chat's")
	jobs := flag.Int("jobs", defaultJobs, "compactions running at once, the chat's before an import's")
	perm := flag.String("permission-mode", "auto", "Claude Code permission mode to start in: manual, acceptEdits or auto (Shift+Tab cycles them)")
	tools := flag.String("tools", "Bash,Read,Edit,Write,WebSearch,WebFetch", "Claude Code tools")
	ownConfig := flag.Bool("claude-config", true, "give turns and subagents your Claude Code skills, plugins, MCP servers and settings (never its hooks, CLAUDE.md or memory)")
	claudeMd := flag.Bool("claude-md", true, "with -claude-config, give turns and subagents the CLAUDE.md files of the folders they work in (never ~/.claude/CLAUDE.md)")
	wake := flag.Bool("wake", true, "let Opt set wake-ups: messages to itself at a time, kept in <dir>/wake.jsonl")
	resumeAtReset := flag.Bool("resume-at-reset", true, "when the plan's usage limit stops Opt's turn or a subagent, start a turn at the reset and resume the subagents, from <dir>/resume.json")
	resetGrace := flag.Duration("reset-grace", 90*time.Second, "how long after the reset -resume-at-reset waits")
	autoResume := flag.Bool("auto-resume", true, fmt.Sprintf("as opt-chat starts, resume Opt's turn and the subagents a quit, a crash or /reload cut off mid-task, under a day ago, up to %d times in a row", resumeMax))
	hideEmail := flag.Bool("hide-email", true, "drop your Claude account's email from the context Claude Code adds to calls: always in compactions, and in turns and subagents with -claude-config=false")
	importPath := flag.String("import", "", "import past chats, then chat: a ~/.claude or ~/.codex directory, a session's .jsonl, a Claude or ChatGPT export (.zip), or a text file whose lines become notes")
	importSince := flag.String("import-since", "", "import only messages from this day on (YYYY-MM-DD)")
	importTools := flag.Bool("import-tools", false, "import tool calls and results too")
	importSkip := flag.String("import-skip", "", "skip sessions whose directory (or a chat's title) contains any of these, comma-separated")
	repl := flag.Bool("repl", false, "chat in the terminal instead of the browser")
	addr := flag.String("addr", "", "where the web chat listens, as 127.0.0.1:port (empty: the last run's port when free, else any)")
	open := flag.Bool("open", true, "open the web chat in the browser")
	sounds := flag.Bool("sounds", true, "play what happens (Opt took your message or ended its turn, a subagent started, reported or failed, a question waits, something broke) from the sound pack peon-ping plays, at its volume; peon toggle mutes them")
	recall := flag.Int("recall", 8, "messages from the whole log that share your message's rare words, given raw to each turn (0: none)")
	evalPath := flag.String("eval", "", "ask this file's questions (JSON lines with \"q\") about the chat as side questions, write the answers to -eval-out, and quit")
	evalOut := flag.String("eval-out", "answers.jsonl", "where -eval writes its answers")
	evalPar := flag.Int("eval-par", 4, "questions -eval asks at once")
	flag.Parse()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1) // at once: unhandled, it would end opt-chat; a reload waits for the start's end
	given := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	// What the model picker saved, unless a flag says otherwise.
	saved := loadSettings(*dir)
	if !given["model"] && modelName.MatchString(saved.Model) {
		*model = saved.Model
	}
	if !given["effort"] && validEffort(saved.Effort) {
		*effort = saved.Effort
	}
	if *jobs < 1 {
		fatal("-jobs must be 1 or more")
	}
	if *agentCap != 0 && (*agentCap < windowMin || *agentCap > windowMax) {
		fatal("-agent-compact-max must be 0, or from %d to %d", windowMin, windowMax)
	}
	if !validMode(*perm) {
		fatal("-permission-mode must be one of %s", strings.Join(modes, ", "))
	}
	for name, e := range map[string]string{"effort": *effort, "agent-effort": *aeffort, "compact-effort": *ceffort} {
		if !validEffort(e) {
			fatal("-%s must be one of %s", name, joinEfforts())
		}
	}
	if os.Getenv(probeEnv) != "" { // a reload's check that this build starts with these flags
		return
	}

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
	agentsMD := filepath.Join(*dir, "AGENTS.md")
	hb := newHub()
	if *sounds {
		hb.sounds = newSounder(peonDir())
	}
	if *repl {
		hb.print = replPrinter()
	} else {
		hb.print = webPrinter()
	}
	h := &harness{}
	a := &Agent{chat: c, system: systemPrompt(agentsMD, *wake), agentsMD: agentsMD, claudeMd: *claudeMd, model: *model, effort: *effort,
		amodel: *amodel, aeffort: *aeffort, cmodel: *cmodel, ceffort: *ceffort, cbare: *cbare, perm: *perm, cwd: *cwd, tools: *tools, ownConfig: *ownConfig, mcpOff: saved.MCPOff, mcpAgents: saved.MCPAgents, skillsSet: saved.Skills, pluginsSet: saved.Plugins, agentWindow: saved.AgentAutocompact, agentCap: *agentCap, recall: *recall,
		running: map[string]*proc{}, writing: map[string]chan struct{}{}, report: func(text string) { h.input(message{kind: "work", text: text}) }, autoResume: *autoResume}
	a.life, a.stop = context.WithCancel(context.Background())
	h.agent = a
	if *hideEmail {
		if a.noEmail, err = writeEmailPlugin(*dir); err != nil {
			warn("the plugin that hides your email: %v", err)
		}
	}
	if a.mcpConfig, a.agentMCP, err = serveMCP(a); err != nil {
		fatal("mcp: %v", err)
	}
	if *evalPath != "" { // no turn, no compaction and no subagent: the chat as it stands
		err := runEval(a, *evalPath, *evalOut, *evalPar)
		a.quit()
		if err != nil {
			fatal("eval: %v", err)
		}
		return
	}
	recovered, cut := a.recoverAgents()
	logAll(c, recovered) // in the chat, for the next turn: none starts now

	c.mu.Lock()
	c.hub = hb
	c.jobs = *jobs
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
	dim(fmt.Sprintf("[%d messages; the view is %d lines, %d bytes; %s%s]", len(c.msgs), len(c.view.keys), c.view.size(), *dir, backlog))
	c.pump()
	c.mu.Unlock()
	a.resumeCut(cut)
	if note, resume := a.recoverTurn(); resume { // a turn starts on it
		h.input(message{kind: "work", text: note})
	} else if note != "" {
		c.Log("work", note)
	}
	rl, err := newReloader(flag.CommandLine)
	if err != nil {
		warn("reload: %v", err)
	}
	reload := func() {
		if rl == nil {
			return
		}
		if err := h.reload(rl); err != nil {
			hb.notice("warn", "reload: %v", err)
			hb.sound(soundError)
		}
	}
	if rl != nil {
		a.onReload = func() { go reload() }
	}
	if *wake { // after the hub: one missed while down starts a turn now
		w, err := startWaker(*dir, func(text string) { h.input(message{kind: "work", text: text}) })
		if err != nil {
			fatal("wake: %v", err)
		}
		a.mu.Lock()
		a.wakes = w
		a.mu.Unlock()
		defer w.stop()
	}
	if *resumeAtReset {
		r, err := startResumer(*dir, a.resumeAfterLimit)
		if err != nil {
			fatal("resume.json: %v", err)
		}
		a.mu.Lock()
		a.resumes, a.resetGrace = r, *resetGrace
		a.mu.Unlock()
		defer r.stop()
	}
	stopStatus := make(chan struct{})
	go watchStatus(c, a, hb, stopStatus)
	hb.sound(soundStart)

	quit := func() {
		c.mu.Lock()
		c.compact = nil // none starts from here on; the next run builds what is left
		c.mu.Unlock()
		h.quit()
		close(stopStatus)
	}
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // the terminal closed: quit as well, stopping every call
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
		for s := range sig {
			if s != syscall.SIGUSR1 {
				break
			}
			go reload()
		}
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
			if q := a.firstAsk(); q != nil { // the line answers the question
				switch strings.ToLower(strings.TrimSpace(text)) {
				case "y", "yes", "1":
					a.answer(q.ID, true, false)
					continue
				case "a", "always", "2":
					a.answer(q.ID, true, q.Always != "")
					continue
				case "", "n", "no", "3":
					a.answer(q.ID, false, false)
					continue
				}
				a.answer(q.ID, false, false) // anything else: no, and the line is what to do instead
			}
			if out, ok, err := command(a, text); err != nil {
				dim(err.Error() + ".")
			} else if ok {
				dim(out)
			} else if strings.TrimSpace(text) != "" {
				h.input(typed(c, text))
			}
		case s := <-sig:
			if s == syscall.SIGUSR1 {
				go reload()
				continue
			}
			back, ok := h.stop()
			if !ok || s != os.Interrupt { // only Ctrl-C stops a turn and stays
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

// systemPrompt is the prompt for a chat whose instructions are in
// agentsMD, with the wake tool listed when on.
func systemPrompt(agentsMD string, wake bool) string {
	s := strings.ReplaceAll(prompt, "{{AGENTS.md}}", agentsMD)
	if wake {
		s = strings.Replace(s, "\n\n# Turns\n", "\n"+wakePrompt+"\n# Turns\n", 1)
	}
	return s
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
	agent     *Agent
	mu        sync.Mutex
	queue     []queued
	lastID    int
	turn      *Turn
	cancel    context.CancelFunc
	running   bool
	stopped   bool // the turn underway was stopped, and its queue taken
	hurry     bool // sent now: the next turn waits for no summary
	quitting  bool
	loops     sync.WaitGroup
	reloading sync.Mutex // held by a reload: see reload.go
}

type queued struct {
	id int
	message
}

func (h *harness) input(m message) {
	if h.aside(m) {
		return
	}
	if m.kind == "user" {
		h.agent.chat.hub.sound(soundAck)
	}
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
		if m.kind == "user" { // the user's turn now: it may set wake-ups
			h.agent.setWoken(false)
		}
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enqueue(m)
}

// inputNow queues m and stops the turn underway, or the wait before it, so
// that m goes at once: send now.
func (h *harness) inputNow(m message) {
	if h.aside(m) {
		return
	}
	if m.kind == "user" {
		h.agent.chat.hub.sound(soundAck)
	}
	h.mu.Lock()
	t := h.turn
	if h.quitting {
		t = nil
	}
	h.mu.Unlock()
	// While a tool call runs, m joins the call and the call is cut, so
	// that the model reads it now, in the same call (see Turn.hurry).
	if t != nil && m.kind == "user" && t.inTool() && t.Send(&m) {
		h.agent.setWoken(false)
		t.hurry()
		return
	}
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
			if it.kind == "user" {
				h.agent.setWoken(false)
			}
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
// next one at once with the queue; with none queued, it cuts the tool calls
// underway for the user's messages that wait for their results. False when
// nothing waits.
func (h *harness) sendNow() bool {
	h.mu.Lock()
	t := h.turn
	if !h.running || !slices.ContainsFunc(h.queue, func(q queued) bool { return q.kind == "user" }) {
		h.mu.Unlock()
		return t != nil && t.hurry() // the user's messages that wait for a tool call's result
	}
	defer h.mu.Unlock()
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
			h.agent.turnUnbegun(h.quitting)
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
		h.agent.setWoken(wakeOnly(msgs))
		h.agent.turnBegan()
		err := t.Run(ctx, msgs)
		if err != nil && ctx.Err() == nil {
			c.hub.notice("warn", "turn: %v", err)
			c.hub.sound(soundError)
		}
		h.agent.setWoken(false)
		h.mu.Lock()
		cut := ctx.Err() != nil && !t.answered // stopped after its answer, as it exited: not cut
		quitting := h.quitting && cut && !h.stopped
		h.agent.turnEnded(quitting)
		if note := endNote(err, cut, h.stopped, quitting); note != "" { // for Opt's next turn
			if err != nil && !cut {
				note = strings.TrimSpace(note + " " + h.agent.limitHit(err.Error(), ""))
			}
			c.Log("work", note)
		} else if err == nil && !cut {
			h.agent.limitWent("")
		}
		h.turn = nil
		var left []queued
		for _, m := range t.detach() { // taken, and logged, but the call ended before it got them
			h.lastID++
			left = append(left, queued{h.lastID, m})
		}
		if !h.stopped { // stopped: they stay in the log, unanswered, as stop says
			h.queue = append(left, h.queue...)
		}
		if err == nil && !cut && !h.stopped && len(h.queue) == 0 { // done, and no turn follows: the user's turn
			c.hub.sound(soundDone)
		}
		h.mu.Unlock()
		cancel()
	}
}

var (
	commandWord = regexp.MustCompile(`(?s)^\s*/([A-Za-z][\w:.-]*)(?:\s+(.*?))?\s*$`) // "/word args", never a path like /Users/me/shot.png
	modelName   = regexp.MustCompile(`^[\w.:\[\]-]+$`)
	commands    = []string{"model", "effort", "mode", "autocompact", "mcp", "skills", "plugins", "agents", "status", "usage", "btw", "reload", "help"}
)

// command runs a line that starts with /word, and says what it did; false
// for a message, which a /word naming one of the user's skills is: Claude
// Code runs the skill. Any other /word is not sent either, as in UniiChat: a
// typo never reaches the chat for good. An error: nothing was done.
func command(a *Agent, line string) (string, bool, error) {
	m := commandWord.FindStringSubmatch(line)
	if m == nil {
		return "", false, nil
	}
	word, arg := strings.ToLower(m[1]), m[2]
	if !slices.Contains(commands, word) {
		switch st, _ := a.skillState(m[1], false); st {
		case "":
			return "", true, fmt.Errorf("/%s is no command; nothing was sent. /help lists the commands", m[1])
		case "off":
			return "", true, fmt.Errorf("/%s is a skill turned off for Opt; nothing was sent. /skills turns it on", m[1])
		}
		return "", false, nil
	}
	if word == "btw" { // a message, which the harness answers aside
		if arg == "" {
			return "", true, errors.New("/btw takes a question, as /btw what was that port again")
		}
		return "", false, nil
	}
	if word == "mcp" || word == "skills" || word == "plugins" { // asks Claude Code: outside a.mu
		return configCommand(a, word, arg), true, nil
	}
	if word == "status" { // asks Claude Code: outside a.mu
		return statusCommand(a), true, nil
	}
	if word == "usage" {
		out, err := usageCommand(a, strings.ToLower(strings.TrimSpace(arg)))
		return out, true, err
	}
	if word == "autocompact" {
		return autocompact(a, arg)
	}
	if word == "reload" {
		if a.onReload == nil {
			return "", true, errors.New("/reload needs opt-chat run from its built binary")
		}
		a.onReload()
		return "Reloading: building opt-chat from its source. Once the chat is idle, it restarts in place, and the subagents it cuts off go on.", true, nil
	}
	if word == "mode" && arg != "" {
		if err := a.setMode(arg); err != nil {
			return "", true, err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case word == "model" && modelName.MatchString(arg):
		a.model = arg
	case word == "model" && arg != "":
		return "", true, errors.New("a model is one word, as for -model: fable, opus, sonnet, or a full name")
	case word == "effort" && validEffort(arg):
		a.effort = arg
	case word == "effort" && arg != "":
		return "", true, errors.New("effort must be one of " + joinEfforts())
	case word == "model" || word == "effort" || word == "mode":
	case word == "agents":
		names, resting := slices.Sorted(maps.Keys(a.running)), slices.Sorted(maps.Keys(a.resting))
		out := "No subagent is at work."
		if len(names) > 0 {
			out = "At work: [" + strings.Join(names, "], [") + "]."
		}
		if len(resting) > 0 {
			out += " Reported, and can go on when told: [" + strings.Join(resting, "], [") + "]."
		}
		if len(names)+len(resting) > 0 {
			out += " Each one's steps: " + filepath.Join(a.chat.dir, "agents", "<Name>.log")
		}
		return out, true, nil
	case word == "help":
		return "/model [name]       show or set the model for turns, as -model (until quit)\n" +
			"/effort [level]     show or set their effort: " + joinEfforts() + "\n" +
			"/mode [mode]        show or set the permission mode: " + strings.Join(modes, ", ") + "\n" +
			"/autocompact [tokens|off] compact a subagent past this context, as 250k, between two steps (never the chat's)\n" +
			"                    the -agent-compact-max cap applies first; the page's compact compacts one now\n" +
			"/mcp [on|off name]  your Claude Code MCP servers: list them, or turn one on or off for Opt\n" +
			"/skills [on|off name] your Claude Code skills: list them, or turn one on or off for Opt\n" +
			"/plugins [on|off id] your Claude Code plugins (skills and MCP servers together), as name@marketplace\n" +
			"/agents             list the subagents at work\n" +
			"/status             Claude Code's version and account, this run's settings, the view and the plan's usage\n" +
			"/usage [day|week|run] the plan's limits, what uses them, and what Opt's calls spent\n" +
			"/btw question       a side question: answered beside the chat, which never sees it\n" +
			"/reload             build opt-chat from its source and, once the chat is idle, restart it in place; cut-off subagents go on\n" +
			"/<skill> [args]     run one of your skills, as in Claude Code\n" +
			"Ctrl-C stops the turn; Ctrl-C when idle, or Ctrl-D, quits.", true, nil
	}
	return fmt.Sprintf("Turns: %s, effort %s, mode %s.", a.model, a.effort, a.perm), true, nil
}

// asks says whether command would ask Claude Code, which can take seconds:
// for its config, or for a skill not listed lately.
func asks(a *Agent, line string) bool {
	m := commandWord.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	switch word := strings.ToLower(m[1]); word {
	case "mcp", "skills", "plugins", "status", "usage":
		return true
	default:
		_, known := a.skillState(m[1], true)
		return !slices.Contains(commands, word) && !known
	}
}

const windowMin, windowMax = 100_000, 1_000_000 // a subagent's compaction window

// agentWindowLocked is the window a subagent compacts past, its own (spawn's
// or the user's) under the cap; 0: never. a.mu held.
func (a *Agent) agentWindowLocked(own int) int {
	switch {
	case own == 0:
		return a.agentCap
	case a.agentCap == 0:
		return own
	}
	return min(own, a.agentCap)
}

// autocompact shows or sets the context past which a subagent is compacted,
// between two steps, for good (settings.json); -agent-compact-max caps it.
// The chat's own calls never compact: see start.
func autocompact(a *Agent, arg string) (string, bool, error) {
	if arg = strings.ToLower(strings.TrimSpace(arg)); arg != "" {
		n := 0
		if arg != "off" {
			num, mult := arg, 1
			if s, ok := strings.CutSuffix(num, "k"); ok {
				num, mult = s, 1_000
			} else if s, ok := strings.CutSuffix(num, "m"); ok {
				num, mult = s, 1_000_000
			}
			f, err := strconv.ParseFloat(num, 64)
			if n = int(f * float64(mult)); err != nil || n < windowMin || n > windowMax {
				return "", true, fmt.Errorf("autocompact is off, or a window from %dk to %dk tokens, as 250k", windowMin/1000, windowMax/1000)
			}
		}
		a.mu.Lock()
		a.agentWindow = n
		a.mu.Unlock()
		if err := a.updateSettings(func(s *settings) { s.AgentAutocompact = n }); err != nil {
			return "", true, err
		}
	}
	a.mu.Lock()
	n, most := a.agentWindow, a.agentCap
	w := a.agentWindowLocked(n)
	a.mu.Unlock()
	capped := ""
	if most > 0 && most < n {
		capped = fmt.Sprintf(", not %dk: -agent-compact-max caps it", n/1000)
	}
	switch {
	case w == 0:
		return "Subagents' contexts never compact (/autocompact 250k: past 250k tokens). The chat's own never do.", true, nil
	case n == 0:
		return fmt.Sprintf("A subagent's context is compacted past the cap, %dk tokens, between two steps, never in one (/autocompact 250k: sooner). The chat's own never compact.", w/1000), true, nil
	}
	return fmt.Sprintf("A subagent's context is compacted past %dk tokens%s, between two steps, never in one; at rest, its next message brings the view and its task again. The chat's own never compact.", w/1000, capped), true, nil
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
				case kindAI:
					dim("[" + kindAI + "] " + oneLine(m.Text, 200))
				case "tool":
					dim("→ " + oneLine(m.Text, 200))
				case "echo":
					dim("  " + oneLine(m.Text, 160))
				}
			}
		case asideUpdate:
			if a := u.Aside; a.Err != "" {
				warn("[btw: %s]", a.Err)
			} else if a.A != "" {
				dim("[btw] " + a.A)
			}
		case thoughtUpdate:
			dim(u.S)
		case asksUpdate:
			if len(u.Asks) > 0 {
				q := u.Asks[0]
				who := "Opt"
				if q.Who != "" {
					who = "[" + q.Who + "]"
				}
				s := fmt.Sprintf("%s asks to run %s %s", who, q.Tool, oneLine(string(q.Input), 300))
				if q.Desc != "" {
					s += " (" + q.Desc + ")"
				}
				s += "\n  y: yes"
				if q.Always != "" {
					s += " · a: yes, and don't ask again for " + q.Always
				}
				warn("%s · n: no · anything else: no, and it goes to Opt", s)
			}
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
	stopChildren(nil)
	os.Exit(1)
}
