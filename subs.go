package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The page's agents rail: each subagent of this run, what it is doing, and
// its steps as its log has them, as they come.

// sub is a subagent as the rail shows it.
type sub struct {
	Name    string   `json:"name"`
	Model   string   `json:"model"`
	Effort  string   `json:"effort"`
	State   string   `json:"state"`            // starting, working, compacting, resting, stopping, ended, failed, stopped (by a quit or crash), halted (by the user)
	Context int      `json:"context,omitzero"` // tokens its latest request read
	Window  int      `json:"window,omitzero"`  // compacted past this, between two steps; 0: never
	Started int64    `json:"started"`          // unix ms: its task, or the tell that set it to work again
	Until   int64    `json:"until,omitzero"`   // resting: unix ms its call ends at, unless told
	Step    string   `json:"step,omitzero"`    // its latest tool call
	Steps   int      `json:"steps,omitzero"`   // its tool calls
	StepAt  int64    `json:"stepAt,omitzero"`  // unix ms of the latest
	User    bool     `json:"user,omitzero"`    // its turn underway, or its last, is the user's: it answers the user, not Opt
	Compact bool     `json:"compact,omitzero"` // the page asked it to compact at its next step
	Phase   string   `json:"phase,omitzero"`   // at work: thinking, writing or tool, as a turn's
	Tool    string   `json:"tool,omitzero"`    // in phase tool: the tool it calls
	MCP     []string `json:"mcp,omitzero"`     // the servers for subagents only Opt granted it
}

// subsKept is how many ended subagents the rail keeps listing.
const subsKept = 8

// subLocked applies f to the subagent name, a.mu held.
func (a *Agent) subLocked(name string, f func(*sub)) {
	if a.subs == nil {
		a.subs = map[string]*sub{}
	}
	s := a.subs[name]
	if s == nil {
		s = &sub{Name: name}
		a.subs[name] = s
	}
	f(s)
}

func (a *Agent) setSub(name string, f func(*sub)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.subLocked(name, f)
}

func done(s *sub) bool { return s.State == "ended" || s.State == "failed" || s.State == "halted" }

// subsLocked lists them, the newest first, the ended ones past subsKept
// forgotten; a.mu held.
func (a *Agent) subsLocked() []sub {
	all := make([]sub, 0, len(a.subs))
	for _, s := range a.subs {
		all = append(all, *s)
	}
	slices.SortFunc(all, func(x, y sub) int { return cmp.Or(cmp.Compare(y.Started, x.Started), cmp.Compare(x.Name, y.Name)) })
	n := 0
	return slices.DeleteFunc(all, func(s sub) bool {
		if !done(&s) {
			return false
		}
		if n++; n > subsKept {
			delete(a.subs, s.Name)
			return true
		}
		return false
	})
}

// step is one entry of a subagent's log: task, on, opt (a tell), user (the
// user's message, from the page), agent (its text), tool, echo (a tool's
// output), loop (its Stop hook's reason) or compacted.
type step struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	At   int64  `json:"at,omitzero"`  // unix ms it was written; 0 in logs from before times were kept
	Ref  string `json:"ref,omitzero"` // a tool call's id, on it and on its echo
}

type (
	stepUpdate struct {
		T    string `json:"t"` // step
		Name string `json:"name"`
		Step step   `json:"step"`
	}
	stepsReply struct {
		T     string     `json:"t"` // steps
		Name  string     `json:"name"`
		Steps []step     `json:"steps"`
		Cut   bool       `json:"cut,omitzero"` // the log's start is left out
		Live  *agentLive `json:"live,omitzero"`
	}
	agentDeltaUpdate struct {
		T    string `json:"t"` // agentDelta
		Name string `json:"name"`
		K    string `json:"k,omitzero"` // text, thinking or input; none, with R: all of them
		S    string `json:"s,omitzero"`
		R    bool   `json:"r,omitzero"` // it starts again: what came before goes
	}
)

// agentLive is what a subagent at work is writing now, which streams to
// the pages as a turn's reply does (--include-partial-messages): its reply,
// its thinking and a tool call's input, each until its step is logged.
type agentLive struct {
	Text     string `json:"text,omitzero"`
	Thinking string `json:"thinking,omitzero"`
	Input    string `json:"input,omitzero"`
}

// agentDelta adds s to what the subagent name is writing, of kind k, or
// with reset writes it anew; with neither k nor s, it has stopped writing.
func (a *Agent) agentDelta(name, k, s string, reset bool) {
	if s == "" && !reset {
		return
	}
	a.mu.Lock()
	if a.subLive == nil {
		a.subLive = map[string]*agentLive{}
	}
	w := a.subLive[name]
	if w == nil {
		w = &agentLive{}
		a.subLive[name] = w
	}
	f := w.field(k)
	switch {
	case f == nil:
		delete(a.subLive, name)
	case reset:
		*f = s
	default:
		*f += s
	}
	a.mu.Unlock()
	a.chat.hub.send(agentDeltaUpdate{T: "agentDelta", Name: name, K: k, S: s, R: reset})
}

func (w *agentLive) field(k string) *string {
	return map[string]*string{"text": &w.Text, "thinking": &w.Thinking, "input": &w.Input}[k]
}

// liveLogged drops what the subagent name was writing of kind k, now a
// step in its log, and gives it; the pages drop it as the step comes.
func (a *Agent) liveLogged(name, k string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	w := a.subLive[name]
	if w == nil {
		return ""
	}
	s := *w.field(k)
	*w.field(k) = ""
	return s
}

// liveOf is what the subagent name is writing now, for a page that opens
// it; nil: nothing.
func (a *Agent) liveOf(name string) *agentLive {
	a.mu.Lock()
	defer a.mu.Unlock()
	if w := a.subLive[name]; w != nil && *w != (agentLive{}) {
		c := *w
		return &c
	}
	return nil
}

// stepsMax is how much of a log the rail gets at first: its end.
const stepsMax = 512 << 10

// stepStart is an entry's head: its kind, then, since times were kept,
// "@" and the unix ms it was written, and on a tool call and its echo "#"
// and the call's id; "tool@1761134400000#toolu_01: ". Logs from before
// have the kind alone, "tool: ".
var stepStart = regexp.MustCompile(`(?m)^(task|on|opt|user|agent|thinking|tool|echo|loop|compacted|cut)(?:@(\d+)(?:#(\S+?))?)?: `)

// stepHead is s's head in the log, as stepStart reads it.
func stepHead(s step) string {
	h := s.Kind
	if s.At > 0 {
		h += "@" + strconv.FormatInt(s.At, 10)
		if s.Ref != "" && !strings.ContainsAny(s.Ref, " \t\r\n") {
			h += "#" + s.Ref
		}
	}
	return h + ": "
}

// steps reads the log of the subagent name: its end, at most stepsMax.
func (a *Agent) steps(name string) ([]step, bool, error) {
	f, err := os.Open(a.agentLog(name))
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	cut := fi.Size() > stepsMax
	if cut {
		if _, err := f.Seek(-stepsMax, io.SeekEnd); err != nil {
			return nil, false, err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	if cut { // from the first entry that starts in it
		i := bytes.Index(b, []byte("\n\n"))
		if i < 0 {
			return []step{}, true, nil
		}
		b = b[i+2:]
	}
	return parseSteps(b), cut, nil
}

// parseSteps splits a log into its entries, each "kind: text" and a blank
// line; a text's own blank line followed by such a start is taken for one.
func parseSteps(b []byte) []step {
	at := stepHeads(b)
	out := []step{}
	for j, m := range at {
		end := len(b)
		if j+1 < len(at) {
			end = at[j+1][0]
		}
		s := step{Kind: string(b[m[2]:m[3]]), Text: string(bytes.TrimSuffix(b[m[1]:end], []byte("\n\n")))}
		if m[4] >= 0 {
			s.At, _ = strconv.ParseInt(string(b[m[4]:m[5]]), 10, 64)
		}
		if m[6] >= 0 {
			s.Ref = string(b[m[6]:m[7]])
		}
		out = append(out, s)
	}
	return out
}

// stepHeads are the heads of a log's entries, as stepStart's submatch
// indexes: each at the log's start or after a blank line.
func stepHeads(b []byte) [][]int {
	var at [][]int
	for _, m := range stepStart.FindAllSubmatchIndex(b, -1) {
		if m[0] == 0 || bytes.HasSuffix(b[:m[0]], []byte("\n\n")) {
			at = append(at, m)
		}
	}
	return at
}

// plainSteps is a log with each entry's head its kind alone, as logs had
// before times were kept, and its thinking left out: what Opt reads of a
// subagent, what it did.
func plainSteps(b []byte) []byte {
	var out []byte
	last, skip := 0, false
	for _, m := range stepHeads(b) {
		if !skip {
			out = append(out, b[last:m[0]]...)
		}
		if skip = string(b[m[2]:m[3]]) == "thinking"; skip {
			continue
		}
		out = append(out, b[m[2]:m[3]]...)
		out = append(out, ": "...)
		last = m[1]
	}
	if skip {
		return out
	}
	return append(out, b[last:]...)
}

func nowMs() int64 { return time.Now().UnixMilli() }

type (
	section struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	contextReply struct {
		T        string    `json:"t"` // agentContext
		Name     string    `json:"name"`
		Sections []section `json:"sections"`
	}
)

// agentContext is what the page's full-screen view of the subagent name
// shows of its context, as far as opt-chat knows it: its task, model and
// size, the first message it was given and, once compacted, its summary
// and the message after it, from its Claude Code session, and where its
// files are. False when there is no such subagent.
func (a *Agent) agentContext(name string) (contextReply, bool) {
	rec, err := a.loadRecord(name)
	a.mu.Lock()
	var s sub
	live := a.subs[name] != nil
	if live {
		s = *a.subs[name]
	}
	user := a.userTurn[name]
	a.mu.Unlock()
	if name == "" || err != nil && !live {
		return contextReply{}, false
	}
	out := contextReply{T: "agentContext", Name: name, Sections: []section{}}
	add := func(title, text string) {
		if text = strings.TrimSpace(text); text != "" {
			out.Sections = append(out.Sections, section{title, text})
		}
	}
	add("Task", rec.Task)
	on := cmp.Or(rec.Model, s.Model)
	if e := cmp.Or(rec.Effort, s.Effort); e != "" {
		on += ", effort " + e
	}
	m := []string{on, "State: " + cmp.Or(s.State, rec.State)}
	if s.Context > 0 {
		m = append(m, fmt.Sprintf("Context: %s tokens, as its latest request read", k(s.Context)))
	}
	if w := cmp.Or(s.Window, rec.Window); w > 0 {
		m = append(m, fmt.Sprintf("Autocompact: past %s tokens, between two steps", k(w)))
	} else {
		m = append(m, "Autocompact: off")
	}
	if rec.Compacted {
		m = append(m, "Compacted: its next message brings the chat's view and its task again")
	}
	if rec.Loop != nil {
		m = append(m, fmt.Sprintf("Loop: %q in %s, at most %d turns", rec.Loop.Command, rec.Loop.Dir, rec.Loop.max()))
	}
	if user {
		m = append(m, "Its turn now answers the user, not Opt")
	}
	add("Model", strings.Join(m, "\n"))
	path := ""
	if rec.Session != "" {
		path = sessionPath(rec.Session)
	}
	if path != "" {
		first, summary, after := sessionContext(path, name)
		add("First message", first)
		add("Compacted summary", summary)
		add("After its compaction", after)
	}
	f := []string{"Log: " + a.agentLog(name), "Record: " + a.recordPath(name)}
	if rec.Session != "" {
		f = append([]string{"Session: " + rec.Session, "Session file: " + cmp.Or(path, "gone from Claude Code's")}, f...)
	}
	add("Files", strings.Join(f, "\n"))
	return out, true
}

// sessionContext reads a subagent's Claude Code session, saved at path:
// the first message it was given (the chat's view, then its task), and
// the latest summary of a compaction, with the message that came after it.
func sessionContext(path, name string) (first, summary, after string) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", ""
	}
	defer f.Close()
	r := bufio.NewReader(f)
	var start []string
	found, users, next := false, 0, false
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			user := bytes.Contains(line, []byte(`"type":"user"`))
			switch {
			case bytes.Contains(line, []byte(`"isCompactSummary":true`)):
				summary, after, next = entryText(line), "", true
			case user && next && !bytes.Contains(line, []byte(`"tool_result"`)):
				after, next = entryText(line), false
			case user && !found && users < 10: // the view's first part, then the rest and the task
				users++
				text := entryText(line)
				start = append(start, text)
				found = strings.Contains(text, "You are subagent ["+name+"]")
			}
		}
		if err != nil {
			break
		}
	}
	if found {
		first = strings.Join(start, "\n\n")
	}
	return first, summary, after
}

// entryText is the text of a message in a session file: its text blocks,
// and each other block as its type.
func entryText(line []byte) string {
	var e struct {
		Message struct {
			Content jsontext.Value `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(e.Message.Content, &s) == nil {
		return s
	}
	var bs []block
	json.Unmarshal(e.Message.Content, &bs)
	var out strings.Builder
	for _, b := range bs {
		if b.Type == "text" {
			out.WriteString(b.Text)
		} else {
			out.WriteString("[" + b.Type + "]")
		}
	}
	return out.String()
}
