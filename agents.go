package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Agents run in the background, so a long task never blocks the chat. An
// agent is a fresh call whose first message is the view, then its task. Its
// own steps stay in its own log, agents/<Name>.log, out of the chat; its
// final reply comes back to the chat as one work message, "[Name] report".

// agentRest is how long a subagent that has reported waits, its context
// whole, for a tell that sets it to work again; then its call ends.
var agentRest = 30 * time.Minute

// restMax is how many rest at once: each is a Claude Code process, a few
// hundred MB. One more ends the call of the one at rest longest.
const restMax = 4

// rest is a subagent that has reported, its call open until then. One
// whose context passed its window is compacted at rest, never mid-step;
// its next message then brings the view and its task again.
type rest struct {
	p          *proc
	until      time.Time
	task       string
	compacting bool     // its /compact is underway: tells wait in pending
	compacted  bool     // since its last message
	pending    []string // tells that came while compacting
}

// k is a count of tokens, as 250k.
func k(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%dk", (n+500)/1000)
}

var badName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// agentName is s as a subagent's name, the same however it is written:
// "[My agent]" is My-agent.
func agentName(s string) string {
	return strings.Trim(badName.ReplaceAllString(strings.Trim(s, "[] "), "-"), "-")
}

func (a *Agent) agentLog(name string) string { return filepath.Join(a.chat.dir, "agents", name+".log") }

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

func validEffort(e string) bool { return slices.Contains(efforts, e) }

func joinEfforts() string { return strings.Join(efforts, ", ") }

// Spawn starts an agent and returns at once. An empty effort is
// -agent-effort; an empty model, -agent-model.
func (a *Agent) Spawn(name, task, effort, model string, window int, loop *loopHook) string {
	if strings.TrimSpace(task) == "" {
		return "No task given."
	}
	if effort != "" && !slices.Contains(efforts, effort) {
		return "effort must be one of " + strings.Join(efforts, ", ") + ", or left out."
	}
	if model != "" && !modelName.MatchString(model) {
		return "model is one word: opus, fable, sonnet, haiku, or a full model name; or left out."
	}
	if window != 0 && (window < windowMin || window > windowMax) {
		return fmt.Sprintf("autocompact must be from %d to %d tokens, or left out.", windowMin, windowMax)
	}
	if loop != nil {
		if bad := loop.check(); bad != "" {
			return bad
		}
	}
	if effort == "" {
		effort = a.aeffort
	}
	if model == "" {
		model = a.amodel
	}
	name = agentName(name)
	if name == "" {
		name = "agent"
	}
	if !a.begin() {
		return "opt-chat is quitting."
	}
	a.mu.Lock()
	base := name
	for n := 2; ; n++ { // names are forever: zoom("Name") opens its log
		if _, taken := a.running[name]; !taken {
			if _, err := os.Stat(a.agentLog(name)); os.IsNotExist(err) {
				break
			}
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	a.running[name] = nil // reserved until its call starts
	a.subLocked(name, func(s *sub) { *s = sub{Name: name, Model: model, Effort: effort, State: "starting", Started: nowMs()} })
	h := a.haltLocked(name)
	a.mu.Unlock()
	go func() {
		defer a.calls.Done() // after its report is in the chat
		a.runAgent(name, h, agentRecord{Session: newSession(), Model: model, Effort: effort, Window: window, Task: task, Loop: loop}, "")
	}()
	return fmt.Sprintf("Started [%s] in the background, on %s. Its report will come as a message starting \"[%s]\".", name, model, name)
}

// Tell hands a running agent a message, between its tool calls, or sets one
// at rest to work again.
func (a *Agent) Tell(name, text string) string {
	name = agentName(name)
	lines := a.chat.viewNow() // in case it was compacted: before a.mu
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.running[name]
	r, resting := a.resting[name]
	switch {
	case !ok && !resting:
		return a.resume(name, text, false)
	case !ok && r.compacting:
		a.agentWrite(name, "opt", text)
		r.pending = append(r.pending, text)
		return fmt.Sprintf("Sent: [%s] is compacting its context; it gets this right after, and will report again.", name)
	case !ok: // at work again; it reports again
		delete(a.resting, name)
		a.running[name], p = r.p, r.p
		a.subLocked(name, func(s *sub) { s.State, s.Started, s.Until = "working", nowMs(), 0 })
		a.note(name, func(r *agentRecord) { r.State = "working" })
		a.chat.hub.notice("info", "[%s] back at work", name)
	case p == nil:
		return fmt.Sprintf("[%s] is starting: tell it again in a moment.", name)
	case a.looping(name):
		a.held[name] = append(a.held[name], text)
		return fmt.Sprintf("Sent: [%s] is in a loop: it gets this when its turn ends, ahead of the loop's next step.", name)
	}
	a.agentWrite(name, "opt", text)
	msg := []block{textBlock(text)}
	if resting && r.compacted {
		msg = anchored(lines, name, r.task, []string{text})
		a.note(name, func(r *agentRecord) { r.Compacted = false })
	}
	if !p.send(msg...) {
		delete(a.running, name)
		return fmt.Sprintf("[%s] has just ended.", name)
	}
	if resting {
		return fmt.Sprintf("Sent: [%s] is at work on it, with all it knew, and will report again.", name)
	}
	return "Sent."
}

// anchored is the message that sets a compacted subagent to work again: the
// view and its task again, then Opt's messages.
func anchored(lines []string, name, task string, msgs []string) []block {
	_, second := viewMessages(lines, []block{textBlock(fmt.Sprintf("Your context was compacted. Above, the chat's view as it is now. "+
		"You are still subagent [%s], and your task was:\n%s\n\nOpt's new message:\n%s", name, task, strings.Join(msgs, "\n\n")))})
	return second
}

func (a *Agent) agentWrite(name, kind, text string) {
	if err := appendRaw(a.agentLog(name), []byte(kind+": "+text+"\n\n")); err != nil {
		warn("[%s] log: %v", name, err)
	}
	a.chat.hub.send(stepUpdate{T: "step", Name: name, Step: step{Kind: kind, Text: text}})
}

// resume sets a subagent whose call has ended to work again: its saved
// session, with all it knew, and text: Opt's message, or, from opt-chat
// (auto), the whole message. a.mu held.
func (a *Agent) resume(name, text string, auto bool) string {
	rec, err := a.loadRecord(name)
	switch {
	case err != nil || rec.Session == "":
		return fmt.Sprintf("No subagent [%s] is taking messages, and none was saved to resume. A new one can read its chat with zoom(%q).", name, name)
	case !saved(rec.Session):
		return fmt.Sprintf("[%s]'s saved session is gone from Claude Code's, so it can't be resumed. A new one can read its chat with zoom(%q).", name, name)
	case a.life.Err() != nil || a.cutting:
		return "opt-chat is quitting."
	}
	msg := resumeNote(rec.State) + text
	if auto {
		msg = text
	}
	a.calls.Add(1) // as begin, a.mu held
	a.running[name] = nil
	a.subLocked(name, func(s *sub) {
		*s = sub{Name: name, Model: rec.Model, Effort: rec.Effort, Window: rec.Window, State: "starting", Started: nowMs()}
	})
	a.agentWrite(name, "opt", text)
	h := a.haltLocked(name)
	go func() {
		defer a.calls.Done()
		a.runAgent(name, h, rec, msg)
	}()
	return fmt.Sprintf("Resuming [%s] where it stopped, from its saved session: it goes on with all it knew, and your message, and will report again.", name)
}

// halt stops one subagent's call: see StopAgent.
type halt struct {
	ctx    context.Context
	cancel context.CancelFunc
	byUser bool // stopped from the page; under a.mu
}

// haltLocked makes the halt of the subagent name's call about to run; a.mu held.
func (a *Agent) haltLocked(name string) *halt {
	ctx, cancel := context.WithCancel(a.life)
	h := &halt{ctx: ctx, cancel: cancel}
	if a.halts == nil {
		a.halts = map[string]*halt{}
	}
	a.halts[name] = h
	return h
}

// StopAgent ends the subagent name's call, as the page's stop asks. One at
// work stops mid-step, and the chat gets a note saying so, not a report:
// the user knows, so no turn starts. One at rest has reported: its call
// just ends. False when it has no call underway.
func (a *Agent) StopAgent(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	h := a.halts[name]
	if h == nil {
		return false
	}
	_, working := a.running[name]
	h.byUser = working
	if r := a.resting[name]; r != nil && !r.compacting {
		r.p.close() // with its input closed, Claude Code exits at once; a SIGTERM takes seconds
	} else {
		h.cancel()
	}
	a.subLocked(name, func(s *sub) { s.State = "stopping" }) // until its call has ended
	return true
}

// ClearAgent drops a subagent whose call has ended from the rail; its log
// stays, for zoom. False when its call is still underway.
func (a *Agent) ClearAgent(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, on := a.halts[name]; on || a.subs[name] == nil {
		return false
	}
	delete(a.subs, name)
	return true
}

// runAgent runs a subagent's call: rec's session anew, its task first, or,
// with a message, resumed. h stops it.
func (a *Agent) runAgent(name string, h *halt, rec agentRecord, msg string) {
	defer func() {
		a.mu.Lock()
		if a.halts[name] == h {
			delete(a.halts, name)
			delete(a.held, name)
		}
		a.mu.Unlock()
		h.cancel()
	}()
	task, effort, model, window := rec.Task, rec.Effort, rec.Model, rec.Window
	resumed := msg != ""
	size := 0 // its context: the tokens its latest request read
	send := func(text string) {
		a.chat.hub.notice("info", "[%s] done", name)
		if !strings.HasPrefix(text, "["+name+"]") { // as the prompt asks of a report
			text = "[" + name + "] " + text
		}
		on := strings.TrimSpace(model + " " + effort)
		if size > 0 {
			on += ", its context at " + k(size) + " tokens"
		}
		a.report(fmt.Sprintf("%s\n\nRan on %s. Full chat: zoom(%q)", text, on, name)) // as UniiChat ends a report
	}
	report := func(text string) { // the end
		a.mu.Lock()
		quit, reload := a.life.Err() != nil, a.cutting
		state := "failed"
		if quit || reload {
			state = "stopped"
		}
		byUser := h.byUser && !quit
		if byUser {
			state = "halted"
		}
		delete(a.running, name)
		a.subLocked(name, func(s *sub) { s.State = state })
		a.mu.Unlock()
		var now agentRecord
		a.note(name, func(r *agentRecord) {
			r.State = state
			r.Cut = state == "stopped" && a.autoResume && r.Resumes < resumeMax && saved(r.Session)
			now = *r
		})
		switch {
		case byUser:
			a.chat.hub.notice("info", "[%s] stopped", name)
			a.chat.Log("work", fmt.Sprintf("[%s] stopped: the user stopped it from the page before it finished, so it sends no report. %s\n\nRan on %s. Full chat: zoom(%q)",
				name, resumeHint(name), strings.TrimSpace(model+" "+effort), name))
			return
		case now.Cut: // the next start resumes it: no report
			a.chat.hub.notice("info", "[%s] cut off: it goes on when opt-chat starts again", name)
			return
		case state == "stopped":
			how, why := "quit", ""
			if !quit {
				how = "reloaded"
			}
			if a.autoResume && now.Resumes >= resumeMax {
				why = fmt.Sprintf(" opt-chat resumed it %d times since its last result, so not again on its own.", resumeMax)
			}
			text = fmt.Sprintf("stopped: opt-chat %s before this subagent finished.%s %s", how, why, resumeHint(name))
		}
		send(text)
	}
	if err := os.MkdirAll(filepath.Join(a.chat.dir, "agents"), 0o700); err != nil {
		report("failed: " + err.Error())
		return
	}
	c := a.chat
	failed := func(err error) string { return "failed: " + err.Error() } // stopped by a quit or a reload: report says so
	var lines []string
	if resumed {
		a.chat.hub.notice("info", "[%s] resumed on %s", name, strings.TrimSpace(model+" "+effort))
	} else {
		a.agentWrite(name, "task", task)
		a.chat.hub.notice("info", "[%s] started on %s", name, strings.TrimSpace(model+" "+effort))
		a.agentWrite(name, "on", strings.TrimSpace(model+" "+effort))
		if err := c.settle(h.ctx, func(int, int) {}); err != nil {
			report(failed(err))
			return
		}
		c.mu.Lock()
		lines = c.callView()
		c.mu.Unlock()
		a.mu.Lock()
		if window == 0 {
			window = a.agentWindow
		}
		a.subLocked(name, func(s *sub) { s.Window = window })
		a.mu.Unlock()
		rec.Window = window
	}
	rec.State = "working"
	a.note(name, func(r *agentRecord) { *r = rec })
	flags := []string{"--session-id", rec.Session}
	if resumed {
		flags = []string{"--resume", rec.Session}
	}
	p, err := a.start(h.ctx, model, effort, roleAgent, flags...)
	if err != nil {
		report(failed(err))
		return
	}
	p.who = name // before its first message: its questions say who asks
	a.note(name, func(r *agentRecord) { r.Pid = p.cmd.Process.Pid })
	acking := !resumed
	each := func(e event) {
		if e.Parent != nil {
			return
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" {
			a.agentWrite(name, "compacted", fmt.Sprintf("%d tokens to %d", e.Compact.Pre, e.Compact.Post))
			a.chat.hub.notice("info", "[%s]'s context was compacted at rest, %s tokens to %s", name, k(e.Compact.Pre), k(e.Compact.Post))
			size = e.Compact.Post
			a.setSub(name, func(s *sub) { s.Context = size })
			return
		}
		if e.Type == "assistant" {
			if u := e.Message.Usage; u.Input+u.Read+u.Write > 0 {
				size = u.Input + u.Read + u.Write
				a.setSub(name, func(s *sub) { s.Context = size })
			}
		}
		for _, b := range e.blocks() {
			switch {
			case e.Type == "assistant" && b.Type == "text" && !acking:
				a.agentWrite(name, "agent", b.Text)
			case e.Type == "assistant" && b.Type == "tool_use":
				call := toolCall(b)
				a.setSub(name, func(s *sub) { s.Step, s.StepAt = head(call, 400), nowMs(); s.Steps++ })
				a.agentWrite(name, "tool", call)
			case e.Type == "user" && b.Type == "tool_result":
				out, _ := resultContent(b.Content)
				a.agentWrite(name, "echo", clip(out, clipMax))
			}
		}
	}
	var u usage
	a.mcpSettle(p)
	var second []block
	switch {
	case resumed && rec.Compacted: // its view and task again, as a tell after a compaction brings them
		second = anchored(c.viewNow(), name, task, []string{msg})
		a.note(name, func(r *agentRecord) { r.Compacted = false })
	case resumed:
		second = []block{textBlock(msg)}
	default:
		var first []block
		first, second = viewMessages(lines, []block{textBlock(fmt.Sprintf("You are subagent [%s]. Your task:\n%s", name, task))})
		if err := a.ack(p, first, &u, each); err != nil {
			p.wait()
			report(failed(err))
			return
		}
	}
	acking = false
	a.mu.Lock()
	p.send(second...)
	a.running[name] = p
	a.subLocked(name, func(s *sub) { s.State = "working" })
	if rec.Loop != nil {
		if a.held == nil {
			a.held = map[string][]string{}
		}
		a.held[name] = nil
	}
	a.mu.Unlock()
	lp := loopState{hook: rec.Loop, turns: rec.Turns}
	for {
		e, ok := p.next()
		if !ok {
			break
		}
		if e.Type != "result" {
			each(e)
			continue
		}
		lines := c.viewNow()
		a.mu.Lock()
		if r := a.resting[name]; r != nil && r.p == p && r.compacting { // the compaction's end, not a report
			r.compacting, r.compacted = false, true
			a.subLocked(name, func(s *sub) { s.State = "resting" })
			a.note(name, func(r *agentRecord) { r.Compacted = true })
			if msgs := r.pending; len(msgs) > 0 { // tells that waited for it
				delete(a.resting, name)
				a.running[name] = p
				a.subLocked(name, func(s *sub) { s.State, s.Started, s.Until = "working", nowMs(), 0 })
				a.note(name, func(r *agentRecord) { r.State, r.Compacted = "working", false })
				p.send(anchored(lines, name, task, msgs)...)
			}
			a.mu.Unlock()
			continue
		}
		if lp.compacting { // its compaction between turns is over: the next comes with the view and its task
			a.mu.Unlock()
			lp.compacting = false
			a.setSub(name, func(s *sub) { s.State = "working" })
			if q := a.takeHeld(name); len(q) > 0 { // Opt's tells first
				lp.next = q
			}
			p.send(anchored(lines, name, task, lp.next)...)
			continue
		}
		final := e.Result
		if e.IsError {
			final = "failed: " + final
		}
		if lp.hook != nil {
			a.mu.Unlock() // the hook takes a while
			next, note := a.loopStep(name, h, &lp, rec.Session, e)
			a.note(name, func(r *agentRecord) { r.Turns, r.Resumes = lp.turns, 0 })
			if next != nil {
				if window > 0 && size > window { // compacted now, between turns, never in one
					lp.compacting, lp.next = true, next
					a.setSub(name, func(s *sub) { s.State = "compacting" })
					p.send(textBlock(compactAsk))
				} else {
					p.send(textBlock(strings.Join(next, "\n\n")))
				}
				continue
			}
			if h.ctx.Err() != nil { // stopped: its call ends now
				continue
			}
			final += note
			a.mu.Lock()
		}
		// At rest: a tell sets it to work again, with all it knew. Past its
		// window, it is compacted now, between steps, never in one.
		if a.resting == nil {
			a.resting = map[string]*rest{}
		}
		delete(a.running, name)
		r := &rest{p: p, until: time.Now().Add(agentRest), task: task, compacting: window > 0 && size > window}
		a.resting[name] = r
		a.subLocked(name, func(s *sub) {
			s.State, s.Until = "resting", r.until.UnixMilli()
			if r.compacting {
				s.State = "compacting"
			}
		})
		if len(a.resting) > restMax {
			oldest := slices.MinFunc(slices.Collect(maps.Keys(a.resting)), func(x, y string) int { return a.resting[x].until.Compare(a.resting[y].until) })
			a.resting[oldest].p.close()
			delete(a.resting, oldest)
			a.subLocked(oldest, func(s *sub) { s.State = "ended" })
		}
		late := a.held[name] // tells that came as its loop ended
		if len(late) > 0 {
			a.held[name] = nil
		}
		a.mu.Unlock()
		a.note(name, func(r *agentRecord) { r.State, r.Resumes = "resting", 0 })
		send(final)
		if len(late) > 0 {
			a.Tell(name, strings.Join(late, "\n\n"))
		}
		if r.compacting {
			p.send(textBlock(compactAsk))
		}
		time.AfterFunc(agentRest, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if r, ok := a.resting[name]; ok && r.p == p && !time.Now().Before(r.until) {
				delete(a.resting, name)
				a.subLocked(name, func(s *sub) { s.State = "ended" })
				p.close() // with its input closed, Claude Code exits
			}
		})
	}
	a.mu.Lock()
	if r := a.resting[name]; r != nil && r.p == p {
		delete(a.resting, name)
	}
	working := a.running[name] == p // its task, or a tell's, left without a reply
	a.subLocked(name, func(s *sub) {
		if !working {
			s.State = "ended"
		}
	})
	a.mu.Unlock()
	if !working {
		a.note(name, func(r *agentRecord) { r.State = "ended" })
	}
	if err := p.wait(); working {
		report(failed(fmt.Errorf("no reply: %v", err)))
	}
}

const compactAsk = "/compact Keep what is needed to go on with your task: what was asked, what is done and how, what is left, the decisions and why, the files and commands."

// ZoomAgent gives a subagent's whole chat.
func (c *Chat) ZoomAgent(name string) string {
	name = agentName(name)
	b, err := os.ReadFile(filepath.Join(c.dir, "agents", name+".log"))
	if name == "" || err != nil {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	return string(b)
}
