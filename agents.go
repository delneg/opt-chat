package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
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
// whose context passed its window is compacted at rest; its next message
// then brings the view and its task again. One the page asked to compact
// at work rests here too while it compacts, between two of its steps, and
// then goes on with its task (goOn).
type rest struct {
	p          *proc
	until      time.Time
	task       string
	compacting bool   // its /compact is underway: tells wait in pending
	compacted  bool   // since its last message
	goOn       bool   // compacting mid-task: after it, it goes on with its task
	pending    []tell // tells that came while compacting
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
	most := cmp.Or(a.agentCap, windowMax) // spawn's window is under the cap
	if window != 0 && (window < windowMin || window > most) {
		return fmt.Sprintf("autocompact must be from %d to %d tokens, or left out.", windowMin, most)
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
		a.runAgent(name, h, agentRecord{Session: newSession(), Model: model, Effort: effort, Window: window, Task: task, Loop: loop}, nil)
	}()
	return fmt.Sprintf("Started [%s] in the background, on %s. Its report will come as a message starting \"[%s]\".", name, model, name)
}

// tell is a message for a subagent: Opt's, by the tell tool, or the
// user's, from the page, where they talk to it as to Opt. A turn that the
// user's message set going (the subagent was not at work) answers the user
// alone: its end reports nothing to Opt. A turn on its task or on Opt's
// tell reports at its end, the user's messages in it or not, and so does
// the user's turn once Opt tells it something. (UniiChat's agent tabs show
// an agent with the input off, https://github.com/VictorTaelin/UniiChat;
// here the user talks to it.)
type tell struct {
	user   bool    // the user's, from the page
	text   string  // as its log has it
	msg    []block // the user's, as the subagent gets it: text, images, files
	logged bool    // in its log already: sent now (tellNow), its turn ended first
}

func (t tell) kind() string {
	if t.user {
		return "user"
	}
	return "opt"
}

const (
	optHead  = "Opt's new message:\n"
	userHead = "The user's message to you, from the page:\n"
)

// tellBlocks is ts as one message: each of the user's after userHead, and
// Opt's as they are, the first after head, one after the user's after
// optHead.
func tellBlocks(ts []tell, head string) []block {
	var out []block
	for i, t := range ts {
		if i > 0 {
			out = append(out, textBlock("\n\n"))
		}
		switch {
		case t.user:
			out = append(append(out, textBlock(userHead)), t.msg...)
		case i == 0:
			out = append(out, textBlock(head+t.text))
		case ts[i-1].user:
			out = append(out, textBlock(optHead+t.text))
		default:
			out = append(out, textBlock(t.text))
		}
	}
	return joinText(out)
}

// joinText merges a message's adjacent text blocks and drops empty ones.
func joinText(bs []block) []block {
	var out []block
	for _, b := range bs {
		switch {
		case b.Type == "text" && b.Text == "":
		case b.Type == "text" && len(out) > 0 && out[len(out)-1].Type == "text":
			out[len(out)-1].Text += b.Text
		default:
			out = append(out, b)
		}
	}
	return out
}

func anyOpt(ts []tell) bool { return slices.ContainsFunc(ts, func(t tell) bool { return !t.user }) }

// userTurnLocked notes whether the subagent name's turn underway is the
// user's, in its record too, so that a restart keeps it. a.mu held.
func (a *Agent) userTurnLocked(name string, user bool) {
	if a.userTurn == nil {
		a.userTurn = map[string]bool{}
	}
	a.userTurn[name] = user
	a.subLocked(name, func(s *sub) { s.User = user })
	a.note(name, func(r *agentRecord) { r.User = user })
}

// Tell hands a running agent Opt's message, between its tool calls, or sets
// one at rest to work again.
func (a *Agent) Tell(name, text string) string {
	out, _ := a.tell(agentName(name), tell{text: text})
	return out
}

// TellUser hands the subagent name the user's message from the page, its
// text and attachments, as Tell hands it Opt's, and notes it in the chat,
// starting no turn, so Opt knows what the user told it. With now, one at
// work gets it at once, as Cmd+Enter sends Opt a message: see tellNow.
// False, with why, when it took nothing.
func (a *Agent) TellUser(name, text string, atts []attachment, now bool) (string, bool) {
	name = agentName(name)
	a.mu.Lock()
	_, working := a.running[name]
	_, resting := a.resting[name]
	a.mu.Unlock()
	if _, err := a.loadRecord(name); name == "" || !working && !resting && err != nil {
		return fmt.Sprintf("no subagent [%s]", name), false
	}
	c := a.chat
	m := c.compose(text, atts)
	t := tell{user: true, text: m.full(), msg: c.content(m)}
	out, ok := "", false
	if now {
		out, ok = a.tellNow(name, t)
	}
	if !ok {
		out, ok = a.tell(name, t)
	}
	if ok {
		c.Log("work", userLine(c, name, text, atts), m.images...)
		c.hub.sound(soundAck)
	}
	return out, ok
}

// userLine is the chat's note of the user's message to a subagent: its
// text, then its attachments by name, a voice message with its words.
func userLine(c *Chat, name, text string, atts []attachment) string {
	s := strings.TrimSpace("[user → " + name + "] " + text)
	var names []string
	for _, at := range atts {
		n := at.Orig
		if words, ok := transcript(filepath.Join(c.dir, at.Name)); ok {
			n += fmt.Sprintf(" (its words: %q)", words)
		}
		names = append(names, n)
	}
	if len(names) > 0 {
		s += "\n\nAttached: " + strings.Join(names, ", ")
	}
	return s
}

// tell hands the subagent name ts: see Tell. False when it took none.
func (a *Agent) tell(name string, ts ...tell) (string, bool) {
	lines := a.chat.viewNow() // in case it was compacted: before a.mu
	a.mu.Lock()
	defer a.mu.Unlock()
	opt := anyOpt(ts)
	p, ok := a.running[name]
	r, resting := a.resting[name]
	switch {
	case !ok && !resting:
		return a.resume(name, ts, false)
	case !ok && r.compacting:
		a.logTells(name, ts)
		r.pending = append(r.pending, ts...)
		return fmt.Sprintf("Sent: [%s] is compacting its context; it gets this right after, and will report again.", name), true
	case !ok: // at work again; it reports again, unless the user set it going
		delete(a.resting, name)
		a.running[name], p = r.p, r.p
		a.subLocked(name, func(s *sub) { s.State, s.Started, s.Until = "working", nowMs(), 0 })
		a.note(name, func(r *agentRecord) { r.State = "working" })
		a.userTurnLocked(name, !opt)
		a.chat.hub.notice("info", "[%s] back at work", name)
		a.chat.hub.sound(soundAck)
	case p == nil:
		return fmt.Sprintf("[%s] is starting: tell it again in a moment.", name), false
	case a.looping(name):
		a.logTells(name, ts) // in its log at once, as a tell at work
		for i := range ts {
			ts[i].logged = true
		}
		a.held[name] = append(a.held[name], ts...)
		if opt && a.userTurn[name] {
			a.userTurnLocked(name, false)
		}
		return fmt.Sprintf("Sent: [%s] is in a loop: it gets this when its turn ends, ahead of the loop's next step.", name), true
	case opt && a.userTurn[name]: // Opt's tell in the user's turn: it reports at its end
		a.userTurnLocked(name, false)
	}
	a.logTells(name, ts)
	if h := a.halts[name]; h != nil && !resting { // queued in its turn: see CompactAgent
		h.told++
	}
	msg := tellBlocks(ts, "")
	if resting && r.compacted {
		msg = anchored(lines, name, r.task, tellBlocks(ts, optHead))
		a.note(name, func(r *agentRecord) { r.Compacted = false })
	}
	if !p.send(msg...) {
		delete(a.running, name)
		delete(a.userTurn, name)
		return fmt.Sprintf("[%s] has just ended.", name), false
	}
	if resting {
		return fmt.Sprintf("Sent: [%s] is at work on it, with all it knew, and will report again.", name), true
	}
	return "Sent.", true
}

func (a *Agent) logTells(name string, ts []tell) {
	for _, t := range ts {
		if !t.logged {
			a.agentWrite(name, t.kind(), t.text)
		}
	}
}

// tellNow interrupts the subagent name at work for the user's message t,
// as Cmd+Enter stops Opt's turn for one: the tool calls it has underway
// are cut short, and it goes on with t at once, its task still whoever's
// it was. A tell waits for its step to end, which a long command makes a
// long wait, and in a loop for its turn to end. In a loop, t goes first
// when the cut turn ends, ahead of the Stop hook's next message (see
// loopStep); between two of its turns, the interrupt finds nothing to cut
// and t goes as the next. False when it is not at work in a way an
// interrupt can cut (starting, compacting): t goes as a tell then.
func (a *Agent) tellNow(name string, t tell) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, h, s := a.running[name], a.halts[name], a.subs[name]
	if p == nil || h == nil || s == nil || s.State != "working" {
		return "", false
	}
	if !h.cut && !p.control("opt-now", map[string]any{"subtype": "interrupt"}) {
		return "", false
	}
	h.cut = true
	a.logTells(name, []tell{t})
	t.logged = true
	if a.looping(name) {
		a.held[name] = append(a.held[name], t)
	} else {
		h.now = append(h.now, t)
	}
	return fmt.Sprintf("Sent: [%s] was interrupted to read it now.", name), true
}

// anchored is the message that sets a compacted subagent to work again: the
// view and its task again, then msg, the new messages with their heads.
func anchored(lines []string, name, task string, msg []block) []block {
	_, second := viewMessages(lines, joinText(append([]block{textBlock(fmt.Sprintf("Your context was compacted. Above, the chat's view as it is now. "+
		"You are still subagent [%s], and your task was:\n%s\n\n", name, task))}, msg...)))
	return second
}

func (a *Agent) agentWrite(name, kind, text string) { a.agentWriteRef(name, kind, text, "") }

// agentWriteRef logs a step of the subagent name with when it was written
// and, on a tool call and its echo, the call's id: see stepHead.
func (a *Agent) agentWriteRef(name, kind, text, ref string) {
	s := step{Kind: kind, Text: text, At: nowMs(), Ref: ref}
	if err := appendRaw(a.agentLog(name), []byte(stepHead(s)+text+"\n\n")); err != nil {
		warn("[%s] log: %v", name, err)
	}
	a.chat.hub.send(stepUpdate{T: "step", Name: name, Step: s})
}

// resume sets a subagent whose call has ended to work again: its saved
// session, with all it knew, and ts, Opt's message or the user's; or, from
// opt-chat (auto), ts is the whole message, and the turn stays whoever's
// it was. a.mu held.
func (a *Agent) resume(name string, ts []tell, auto bool) (string, bool) {
	rec, err := a.loadRecord(name)
	switch {
	case err != nil || rec.Session == "":
		return fmt.Sprintf("No subagent [%s] is taking messages, and none was saved to resume. A new one can read its chat with zoom(%q).", name, name), false
	case !saved(rec.Session):
		return fmt.Sprintf("[%s]'s saved session is gone from Claude Code's, so it can't be resumed. A new one can read its chat with zoom(%q).", name, name), false
	case a.life.Err() != nil || a.cutting:
		return "opt-chat is quitting.", false
	}
	msg := joinText(append([]block{textBlock(resumeNote(rec.State))}, tellBlocks(ts, "Opt's message:\n")...))
	if auto {
		msg = tellBlocks(ts, "")
	} else {
		rec.User = !anyOpt(ts)
	}
	a.calls.Add(1) // as begin, a.mu held
	a.running[name] = nil
	a.subLocked(name, func(s *sub) {
		*s = sub{Name: name, Model: rec.Model, Effort: rec.Effort, Window: rec.Window, State: "starting", Started: nowMs()}
	})
	a.userTurnLocked(name, rec.User)
	a.logTells(name, ts)
	h := a.haltLocked(name)
	go func() {
		defer a.calls.Done()
		a.runAgent(name, h, rec, msg)
	}()
	return fmt.Sprintf("Resuming [%s] where it stopped, from its saved session: it goes on with all it knew, and your message, and will report again.", name), true
}

// halt stops one subagent's call: see StopAgent.
type halt struct {
	ctx     context.Context
	cancel  context.CancelFunc
	byUser  bool   // stopped from the page; under a.mu
	compact bool   // the page asked it to compact at its next step; under a.mu
	told    int    // messages sent to it at work, queued in its turn; under a.mu
	now     []tell // the user's, sent now: it was interrupted for them (tellNow); under a.mu
	cut     bool   // interrupted for the user's message, until its turn's result; under a.mu
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
	if r := a.resting[name]; r != nil && r.goOn { // compacting mid-task
		working = true
	}
	h.byUser = working
	if r := a.resting[name]; r != nil && !r.compacting {
		r.p.close() // with its input closed, Claude Code exits at once; a SIGTERM takes seconds
	} else {
		h.cancel()
	}
	a.subLocked(name, func(s *sub) { s.State = "stopping" }) // until its call has ended
	return true
}

// CompactAgent compacts the subagent name's context, as the page's compact
// asks. One at rest is compacted now, as one past its window is. One at
// work is compacted at its next step: once its tool calls have their
// results, it is interrupted there, compacted, and set to go on with its
// task; or as its turn ends, if that comes first. A message queued in its
// turn would run before the /compact, as a turn of its own, so it waits
// two steps past the one it first sees after such a message: Claude Code
// takes a queued message into the request after a step, that one or the
// next. Claude Code runs a /compact sent mid-turn only
// once the turn is over, and a window lowered mid-turn (apply_flag_settings)
// is not read until then: both measured on Claude Code 2.1.304. False,
// with why, when it can't.
func (a *Agent) CompactAgent(name string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	h, r, p, s := a.halts[name], a.resting[name], a.running[name], a.subs[name]
	switch {
	case h == nil:
		return fmt.Sprintf("[%s] has no call underway: nothing to compact.", name), false
	case r != nil && r.compacting, h.compact, s != nil && s.State == "compacting":
		return fmt.Sprintf("[%s] is compacting already.", name), false
	case r != nil:
		r.compacting = true
		a.subLocked(name, func(s *sub) { s.State = "compacting" })
		r.p.send(textBlock(compactAsk))
		return fmt.Sprintf("[%s] compacts its context now.", name), true
	case p == nil:
		return fmt.Sprintf("[%s] is starting: compact it in a moment.", name), false
	}
	h.compact = true
	a.subLocked(name, func(s *sub) { s.Compact = true })
	return fmt.Sprintf("[%s] compacts its context at its next step, then goes on.", name), true
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
func (a *Agent) runAgent(name string, h *halt, rec agentRecord, msg []block) {
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
	resumed := len(msg) > 0
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
		a.subLocked(name, func(s *sub) { s.State, s.Compact = state, false })
		user := a.userTurn[name]
		delete(a.userTurn, name)
		a.mu.Unlock()
		if state == "failed" { // stopped by the user, a quit or a reload: no sound
			a.chat.hub.sound(soundError)
		}
		var now agentRecord
		a.note(name, func(r *agentRecord) {
			r.State = state
			r.Cut = state == "stopped" && a.autoResume && r.Resumes < resumeMax && saved(r.Session)
			now = *r
		})
		switch {
		case now.Cut: // the next start resumes it, the user's turn or not: no report
			a.chat.hub.notice("info", "[%s] cut off: it goes on when opt-chat starts again", name)
			return
		case user: // the user's turn, failed or stopped: the page alone hears of it
			switch {
			case byUser:
				a.chat.hub.notice("info", "[%s] stopped", name)
			case state == "stopped":
				a.chat.hub.notice("info", "[%s] stopped: opt-chat quit or reloaded before it answered you", name)
			default:
				a.chat.hub.notice("warn", "[%s] %s", name, head(text, 400))
			}
			return
		case byUser:
			a.chat.hub.notice("info", "[%s] stopped", name)
			a.chat.Log("work", fmt.Sprintf("[%s] stopped: the user stopped it from the page before it finished, so it sends no report. %s\n\nRan on %s. Full chat: zoom(%q)",
				name, resumeHint(name), strings.TrimSpace(model+" "+effort), name))
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
		a.chat.hub.sound(soundStart)
	} else {
		a.agentWrite(name, "task", task)
		a.chat.hub.notice("info", "[%s] started on %s", name, strings.TrimSpace(model+" "+effort))
		a.chat.hub.sound(soundStart)
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
		a.mu.Unlock()
		rec.Window = window // its own, saved; the cap applies as it runs
	}
	a.mu.Lock()
	window = a.agentWindowLocked(window)
	a.subLocked(name, func(s *sub) { s.Window = window })
	a.mu.Unlock()
	rec.State = "working"
	a.note(name, func(r *agentRecord) { *r = rec })
	flags := []string{"--session-id", rec.Session}
	if resumed {
		flags = []string{"--resume", rec.Session}
	}
	p, err := a.startAgent(h.ctx, model, effort, window, flags...)
	if err != nil {
		report(failed(err))
		return
	}
	p.who = name // before its first message: its questions say who asks
	a.note(name, func(r *agentRecord) { r.Pid = p.cmd.Process.Pid })
	acking := !resumed
	// Past its window, Claude Code compacts its context itself, between two
	// steps. The page's compact interrupts it at a step's end (forced: 1),
	// then compacts it (2). A step ends once each of its tool calls has its
	// result: open counts those without.
	forced, open, told, quiet, midCompacted := 0, 0, 0, 2, false
	phase := func(ph, tool string) { a.setSub(name, func(s *sub) { s.Phase, s.Tool = ph, tool }) }
	cutOpen := func() { // a turn or call ended, with calls underway: the page shows them cut short
		if open > 0 {
			a.agentWrite(name, "cut", "its tool calls underway were cut short")
		}
		open = 0
		phase("", "")
		a.agentDelta(name, "", "", true)
	}
	stepEnd := func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		if h.told != told { // queued in its turn: see CompactAgent
			told, quiet = h.told, 0
		}
		quiet++
		if !h.compact || forced != 0 || quiet < 3 {
			return
		}
		h.compact, forced = false, 1
		a.subLocked(name, func(s *sub) { s.State, s.Compact = "compacting", false })
		p.control("opt-compact", map[string]any{"subtype": "interrupt"})
	}
	each := func(e event) {
		if e.Parent != nil {
			return
		}
		if e.Type == "system" && e.Subtype == "status" {
			if e.Status == "compacting" { // its own, mid-task, or one asked
				a.setSub(name, func(s *sub) {
					if s.State == "working" {
						s.State = "compacting"
					}
				})
			} else { // over, even if it failed: one at work is at work again
				a.mu.Lock()
				if a.running[name] == p && forced == 0 {
					a.subLocked(name, func(s *sub) {
						if s.State == "compacting" {
							s.State = "working"
						}
					})
				}
				a.mu.Unlock()
			}
			return
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" {
			size = e.Compact.Post
			if e.Compact.Trigger == "manual" && forced == 0 { // asked at rest, or between a loop's turns
				a.agentWrite(name, "compacted", fmt.Sprintf("%d tokens to %d", e.Compact.Pre, e.Compact.Post))
				a.chat.hub.notice("info", "[%s]'s context was compacted at rest, %s tokens to %s", name, k(e.Compact.Pre), k(e.Compact.Post))
				a.setSub(name, func(s *sub) { s.Context = size })
				return
			}
			a.agentWrite(name, "compacted", fmt.Sprintf("mid-task: %d tokens to %d", e.Compact.Pre, e.Compact.Post))
			a.chat.hub.notice("info", "[%s]'s context was compacted mid-task, between two steps, %s tokens to %s", name, k(e.Compact.Pre), k(e.Compact.Post))
			a.chat.hub.sound(soundLimit)
			midCompacted = forced == 0 // its next message, after its report, brings the view and its task again
			a.setSub(name, func(s *sub) {
				s.Context = size
				if s.State == "compacting" && forced == 0 {
					s.State = "working"
				}
			})
			return
		}
		if e.Type == "stream_event" { // what it writes, as it writes it, as a turn's reply
			var s streamEvent
			if acking || json.Unmarshal(e.Event, &s) != nil {
				return
			}
			switch s.Type {
			case "message_start":
				phase("thinking", "")
			case "content_block_start":
				switch s.ContentBlock.Type {
				case "text":
					a.agentDelta(name, "text", "", true)
					phase("writing", "")
				case "tool_use":
					a.agentDelta(name, "input", "", true)
					phase("tool", strings.TrimPrefix(s.ContentBlock.Name, "mcp__opt__"))
				case "thinking":
					a.agentDelta(name, "thinking", "", true)
					phase("thinking", "")
				}
			case "content_block_delta":
				switch s.Delta.Type {
				case "text_delta":
					a.agentDelta(name, "text", s.Delta.Text, false)
				case "thinking_delta":
					a.agentDelta(name, "thinking", s.Delta.Thinking, false)
				case "input_json_delta":
					a.agentDelta(name, "input", s.Delta.JSON, false)
				}
			}
			return
		}
		if e.Type == "assistant" {
			if u := e.Message.Usage; u.Input+u.Read+u.Write > 0 {
				size = u.Input + u.Read + u.Write
				a.setSub(name, func(s *sub) { s.Context = size })
			}
		}
		results := 0
		for _, b := range e.blocks() {
			switch {
			case e.Type == "assistant" && b.Type == "thinking" && !acking:
				s := a.liveLogged(name, "thinking") // as it streamed: the block may come without its text
				if b.Thinking != "" {
					s = b.Thinking
				}
				if s != "" {
					a.agentWrite(name, "thinking", s)
				}
			case e.Type == "assistant" && b.Type == "text" && !acking:
				a.liveLogged(name, "text")
				a.agentWrite(name, "agent", b.Text)
			case e.Type == "assistant" && b.Type == "tool_use":
				a.liveLogged(name, "input")
				call := toolCall(b)
				a.setSub(name, func(s *sub) { s.Step, s.StepAt = head(call, 400), nowMs(); s.Steps++ })
				a.agentWriteRef(name, "tool", call, b.ID)
				open++
			case e.Type == "user" && b.Type == "tool_result":
				out, _ := resultContent(b.Content)
				a.agentWriteRef(name, "echo", clip(out, clipMax), b.ToolID)
				open, results = open-1, results+1
			}
		}
		if results > 0 && open <= 0 {
			open = 0
			phase("thinking", "") // with every result in, as a turn's
			stepEnd()
		}
	}
	var u usage
	a.mcpSettle(p)
	var second []block
	switch {
	case resumed && rec.Compacted: // its view and task again, as a tell after a compaction brings them
		second = anchored(c.viewNow(), name, task, msg)
		a.note(name, func(r *agentRecord) { r.Compacted = false })
	case resumed:
		second = msg
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
			a.held = map[string][]tell{}
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
		cutOpen()
		lines := c.viewNow()
		a.mu.Lock()
		h.cut = false
		if r := a.resting[name]; r != nil && r.p == p && r.compacting { // the compaction's end, not a report
			r.compacting, r.compacted = false, true
			a.subLocked(name, func(s *sub) { s.State = "resting" })
			a.note(name, func(r *agentRecord) { r.Compacted = true })
			if msgs := r.pending; len(msgs) > 0 || r.goOn { // tells that waited for it; its task, cut at a step
				delete(a.resting, name)
				a.running[name] = p
				a.subLocked(name, func(s *sub) {
					s.State, s.Until = "working", 0
					if !r.goOn {
						s.Started = nowMs()
					}
				})
				a.note(name, func(r *agentRecord) { r.State, r.Compacted = "working", false })
				var msg []block
				if r.goOn { // the turn stays whoever's it was, unless Opt told it something
					forced = 0
					a.userTurnLocked(name, a.userTurn[name] && !anyOpt(msgs))
					msg = []block{textBlock(goOn)}
					if len(msgs) > 0 {
						msg = joinText(append(append(msg, textBlock("\n\n")), tellBlocks(msgs, optHead)...))
					}
				} else {
					a.userTurnLocked(name, !anyOpt(msgs))
					msg = tellBlocks(msgs, optHead)
				}
				p.send(anchored(lines, name, task, msg)...)
			}
			a.mu.Unlock()
			continue
		}
		if forced == 1 && e.IsError && e.Subtype == "error_during_execution" { // interrupted at a step's end, as asked: compacted, then it goes on
			forced = 2
			delete(a.running, name)
			if a.resting == nil {
				a.resting = map[string]*rest{}
			}
			a.resting[name] = &rest{p: p, until: time.Now().Add(agentRest), task: task, compacting: true, goOn: true, pending: h.now}
			h.now = nil
			a.mu.Unlock()
			p.send(textBlock(compactAsk))
			continue
		}
		if len(h.now) > 0 && e.IsError && e.Subtype == "error_during_execution" { // interrupted for the user's message: it goes on with it
			ts := h.now
			h.now = nil
			a.mu.Unlock()
			p.send(tellBlocks(ts, "")...)
			continue
		}
		// Asked to compact at a step, but its turn ended first: compacted
		// now, as it reports.
		asked := forced == 1 || h.compact
		forced, h.compact = 0, false
		a.subLocked(name, func(s *sub) { s.Compact = false })
		if lp.compacting { // its compaction between turns is over: the next comes with the view and its task
			a.mu.Unlock()
			lp.compacting = false
			a.setSub(name, func(s *sub) { s.State = "working" })
			if q := a.takeHeld(name); len(q) > 0 { // Opt's tells first
				lp.next = q
			}
			p.send(anchored(lines, name, task, tellBlocks(lp.next, optHead))...)
			continue
		}
		final := e.Result
		if e.IsError {
			final = "failed: " + final
			if line := a.limitHitLocked(e.Result, name); line != "" {
				final += " " + line
			}
		} else {
			a.limitWentLocked()
		}
		if lp.hook != nil {
			a.mu.Unlock() // the hook takes a while
			next, note := a.loopStep(name, h, &lp, rec.Session, e)
			a.note(name, func(r *agentRecord) { r.Turns, r.Resumes = lp.turns, 0 })
			if next != nil {
				if window > 0 && size > window || asked { // compacted now, between turns, never in one
					lp.compacting, lp.next = true, next
					a.setSub(name, func(s *sub) { s.State = "compacting" })
					p.send(textBlock(compactAsk))
				} else {
					p.send(tellBlocks(next, "")...)
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
		r := &rest{p: p, until: time.Now().Add(agentRest), task: task, compacting: window > 0 && size > window || asked, compacted: midCompacted}
		mid := midCompacted
		midCompacted = false
		a.resting[name] = r
		a.subLocked(name, func(s *sub) {
			s.State, s.Until = "resting", r.until.UnixMilli()
			if r.compacting {
				s.State = "compacting"
			}
		})
		if len(a.resting) > restMax {
			done := slices.DeleteFunc(slices.Collect(maps.Keys(a.resting)), func(n string) bool { return a.resting[n].goOn }) // not one mid-task
			oldest := slices.MinFunc(done, func(x, y string) int { return a.resting[x].until.Compare(a.resting[y].until) })
			a.resting[oldest].p.close()
			delete(a.resting, oldest)
			a.subLocked(oldest, func(s *sub) { s.State = "ended" })
		}
		late := append(a.held[name], h.now...) // tells that came as its loop ended, or as it ended its turn
		if len(late) > 0 {
			a.held[name], h.now = nil, nil
		}
		user := a.userTurn[name]
		delete(a.userTurn, name)
		a.note(name, func(r *agentRecord) { // under a.mu: a tell now comes after
			r.State, r.Resumes, r.User = "resting", 0, false
			r.Compacted = r.Compacted || mid
		})
		compact := r.compacting // read here: the page's compact may set it once unlocked, and sends its own
		a.mu.Unlock()
		if user { // its reply was the user's, in its log: Opt gets no report
			a.chat.hub.notice("info", "[%s] answered you", name)
		} else {
			send(final)
		}
		a.chat.hub.sound(soundDone)
		if len(late) > 0 {
			a.tell(name, late...)
		}
		if compact {
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
	cutOpen()
	a.mu.Lock()
	working := a.running[name] == p // its task, or a tell's, left without a reply
	if r := a.resting[name]; r != nil && r.p == p {
		working = working || r.goOn // cut off compacting mid-task
		delete(a.resting, name)
	}
	a.subLocked(name, func(s *sub) {
		s.Compact = false
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

// goOn sets a subagent compacted mid-task to work again.
const goOn = "It was compacted in the middle of the task, between two steps: go on with it where you left off."

// ZoomAgent gives a subagent's whole chat, its steps' times and ids left
// out.
func (c *Chat) ZoomAgent(name string) string {
	name = agentName(name)
	b, err := os.ReadFile(filepath.Join(c.dir, "agents", name+".log"))
	if name == "" || err != nil {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	return string(plainSteps(b))
}
