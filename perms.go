package main

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// Permissions, as in Claude Code. The mode says which tool calls go ahead
// unasked: in manual, only reads; in acceptEdits, file edits too; in auto,
// whatever Claude Code's classifier deems safe. Shift+Tab cycles them, and
// the turn and subagents underway switch at once. Any other call waits for
// the user's yes or no: Claude Code asks (can_use_tool) and the page shows
// the question until it is answered.

// modes are those Shift+Tab cycles through, in order.
var modes = []string{"manual", "acceptEdits", "auto"}

func validMode(m string) bool {
	return slices.Contains(modes, m) || m == "default" || m == "bypassPermissions" || m == "dontAsk"
}

// nextMode is the one after m, as Shift+Tab goes.
func nextMode(m string) string {
	if m == "default" { // Claude Code's name for manual
		m = "manual"
	}
	i := slices.Index(modes, m)
	return modes[(i+1)%len(modes)] // from a mode not in the cycle, its first
}

// setMode switches the turns' and subagents' mode, theirs underway too.
func (a *Agent) setMode(m string) error {
	if !validMode(m) {
		return fmt.Errorf("%q is no permission mode: %s", m, strings.Join(modes, ", "))
	}
	a.modeMu.Lock() // the calls get their switches in order: see start
	defer a.modeMu.Unlock()
	a.mu.Lock()
	a.perm = m
	live := make([]*proc, 0, len(a.live))
	for p := range a.live {
		live = append(live, p)
	}
	a.ctlSeq++
	id := fmt.Sprint("opt-mode-", a.ctlSeq)
	a.mu.Unlock()
	for _, p := range live {
		p.control(id, map[string]any{"subtype": "set_permission_mode", "mode": m})
	}
	return nil
}

// ask is a tool call waiting for the user's permission.
type ask struct {
	ID     int            `json:"id"`
	Who    string         `json:"who,omitzero"` // a subagent's name; empty for the turn
	Tool   string         `json:"tool"`
	Input  jsontext.Value `json:"input"`
	Desc   string         `json:"desc,omitzero"`   // what Claude Code says it does
	Always string         `json:"always,omitzero"` // what yes-and-don't-ask-again allows; empty: no such choice

	p           *proc
	reqID       string
	suggestions []suggestion
	answered    chan struct{}
}

// suggestion is a permission change Claude Code offers with its question,
// as "don't ask again for this".
type suggestion struct {
	Type  string `json:"type"` // addRules, setMode, addDirectories
	Rules []struct {
		ToolName    string `json:"toolName"`
		RuleContent string `json:"ruleContent,omitzero"`
	} `json:"rules,omitzero"`
	Behavior    string   `json:"behavior,omitzero"`
	Mode        string   `json:"mode,omitzero"`
	Directories []string `json:"directories,omitzero"`
	Destination string   `json:"destination"`
}

func (s suggestion) rules() []string {
	var out []string
	for _, r := range s.Rules {
		if r.RuleContent != "" {
			out = append(out, r.ToolName+"("+r.RuleContent+")")
		} else {
			out = append(out, r.ToolName)
		}
	}
	return out
}

// always says what the suggestions allow, for the second choice.
func always(ss []suggestion) string {
	var parts []string
	for _, s := range ss {
		switch {
		case s.Type == "addRules" && s.Behavior == "allow":
			parts = append(parts, strings.Join(s.rules(), ", "))
		case s.Type == "setMode" && s.Mode == "acceptEdits":
			parts = append(parts, "all edits (accept edits on)")
		case s.Type == "setMode":
			parts = append(parts, s.Mode+" mode")
		case s.Type == "addDirectories":
			parts = append(parts, "files in "+strings.Join(s.Directories, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// request handles a control request Claude Code sends a turn or subagent.
func (a *Agent) request(p *proc, e event) {
	if e.Type == "control_cancel_request" { // Claude Code asks no more
		a.mu.Lock()
		id := 0
		for _, q := range a.asks {
			if q.p == p && q.reqID == e.RequestID {
				id = q.ID
			}
		}
		a.mu.Unlock()
		if q := a.takeAsk(id); q != nil {
			close(q.answered)
			a.publishAsks()
		}
		return
	}
	var r struct {
		Subtype     string         `json:"subtype"`
		ToolName    string         `json:"tool_name"`
		DisplayName string         `json:"display_name"`
		Input       jsontext.Value `json:"input"`
		Description string         `json:"description"`
		Suggestions []suggestion   `json:"permission_suggestions"`
	}
	if json.Unmarshal(e.Request, &r) != nil || r.Subtype != "can_use_tool" {
		p.reply(e.RequestID, nil, "opt-chat does not take "+cmp.Or(r.Subtype, "this request"))
		return
	}
	tool := strings.TrimPrefix(r.ToolName, "mcp__opt__")
	if r.DisplayName != "" && !strings.HasPrefix(r.ToolName, "mcp__") {
		tool = r.DisplayName
	}
	if len(r.Input) == 0 {
		r.Input = jsontext.Value("{}")
	}
	q := &ask{Who: p.who, Tool: tool, Input: r.Input, Desc: r.Description, Always: always(r.Suggestions),
		p: p, reqID: e.RequestID, suggestions: r.Suggestions, answered: make(chan struct{})}
	a.mu.Lock()
	a.askSeq++
	q.ID = a.askSeq
	a.asks = append(a.asks, q)
	a.mu.Unlock()
	a.publishAsks()
	go func() { // stopped before an answer: the question goes
		select {
		case <-q.answered:
		case <-p.ctx.Done():
			if a.takeAsk(q.ID) != nil {
				a.publishAsks()
			}
		}
	}()
}

func (a *Agent) takeAsk(id int) *ask {
	a.mu.Lock()
	defer a.mu.Unlock()
	i := slices.IndexFunc(a.asks, func(q *ask) bool { return q.ID == id })
	if i < 0 {
		return nil
	}
	q := a.asks[i]
	a.asks = slices.Delete(a.asks, i, i+1)
	return q
}

// publishAsks shows the pages the questions waiting; under a.mu, so that
// two calls can't send their lists out of order (the hub takes no lock of
// the agent's).
func (a *Agent) publishAsks() {
	a.mu.Lock()
	defer a.mu.Unlock()
	items := make([]ask, len(a.asks))
	for i, q := range a.asks {
		items[i] = *q
	}
	a.chat.hub.setAsks(items)
}

// firstAsk is the question the terminal answers next; nil when none waits.
func (a *Agent) firstAsk() *ask {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.asks) == 0 {
		return nil
	}
	return a.asks[0]
}

// answer answers a question: yes, yes and don't ask again (always), or no.
// A no stops the turn, as in Claude Code, for the user to say what to do
// instead; a subagent goes on, told no. False when it is gone.
func (a *Agent) answer(id int, allow, forever bool) bool {
	q := a.takeAsk(id)
	if q == nil {
		return false
	}
	close(q.answered)
	a.publishAsks()
	if !allow {
		r := map[string]any{"behavior": "deny", "message": "The user said no to this call."}
		if q.Who == "" {
			r["interrupt"] = true
			q.p.declined.Store(true)
		}
		q.p.reply(q.reqID, r, "")
		return true
	}
	r := map[string]any{"behavior": "allow", "updatedInput": q.Input}
	if forever && len(q.suggestions) > 0 {
		var perms []suggestion
		for _, s := range q.suggestions {
			s.Destination = "session" // this call's: nothing is written to the user's settings
			perms = append(perms, s)
			switch {
			case s.Type == "addRules" && s.Behavior == "allow":
				a.mu.Lock()
				for _, rule := range s.rules() {
					if !slices.Contains(a.allowed, rule) {
						a.allowed = append(a.allowed, rule)
					}
				}
				a.mu.Unlock()
			case s.Type == "addDirectories":
				a.mu.Lock()
				for _, d := range s.Directories {
					if !slices.Contains(a.dirs, d) {
						a.dirs = append(a.dirs, d)
					}
				}
				a.mu.Unlock()
			case s.Type == "setMode" && validMode(s.Mode):
				a.setMode(s.Mode)
			}
		}
		r["updatedPermissions"] = perms
	}
	q.p.reply(q.reqID, r, "")
	return true
}

// reply answers a control request: with v, or with an error when errMsg is set.
func (p *proc) reply(id string, v any, errMsg string) {
	resp := map[string]any{"subtype": "success", "request_id": id, "response": v}
	if errMsg != "" {
		resp = map[string]any{"subtype": "error", "request_id": id, "error": errMsg}
	}
	b, err := json.Marshal(map[string]any{"type": "control_response", "response": resp})
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		p.stdin.Write(append(b, '\n'))
	}
}
