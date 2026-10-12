package main

import (
	"bytes"
	"cmp"
	"io"
	"os"
	"regexp"
	"slices"
	"time"
)

// The page's agents rail: each subagent of this run, what it is doing, and
// its steps as its log has them, as they come.

// sub is a subagent as the rail shows it.
type sub struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
	State   string `json:"state"`            // starting, working, compacting, resting, ended, failed
	Context int    `json:"context,omitzero"` // tokens its latest request read
	Window  int    `json:"window,omitzero"`  // compacted at rest past this; 0: never
	Started int64  `json:"started"`          // unix ms: its task, or the tell that set it to work again
	Until   int64  `json:"until,omitzero"`   // resting: unix ms its call ends at, unless told
	Step    string `json:"step,omitzero"`    // its latest tool call
	Steps   int    `json:"steps,omitzero"`   // its tool calls
	StepAt  int64  `json:"stepAt,omitzero"`  // unix ms of the latest
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

func (a *Agent) subState(name, state string) {
	a.setSub(name, func(s *sub) { s.State = state })
}

func done(s *sub) bool { return s.State == "ended" || s.State == "failed" }

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

// step is one entry of a subagent's log: task, on, opt (a tell), agent (its
// text), tool, echo (a tool's output) or compacted.
type step struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type (
	stepUpdate struct {
		T    string `json:"t"` // step
		Name string `json:"name"`
		Step step   `json:"step"`
	}
	stepsReply struct {
		T     string `json:"t"` // steps
		Name  string `json:"name"`
		Steps []step `json:"steps"`
		Cut   bool   `json:"cut,omitzero"` // the log's start is left out
	}
)

// stepsMax is how much of a log the rail gets at first: its end.
const stepsMax = 512 << 10

var stepStart = regexp.MustCompile(`(?m)^(task|on|opt|agent|tool|echo|compacted): `)

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
	out := []step{}
	at := stepStart.FindAllSubmatchIndex(b, -1)
	for j, m := range at {
		if m[0] > 0 && !bytes.HasSuffix(b[:m[0]], []byte("\n\n")) {
			continue
		}
		end := len(b)
		for _, n := range at[j+1:] {
			if bytes.HasSuffix(b[:n[0]], []byte("\n\n")) {
				end = n[0]
				break
			}
		}
		out = append(out, step{Kind: string(b[m[2]:m[3]]), Text: string(bytes.TrimSuffix(b[m[1]:end], []byte("\n\n")))})
	}
	return out
}

func nowMs() int64 { return time.Now().UnixMilli() }
