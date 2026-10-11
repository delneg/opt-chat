package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// The user's own Claude Code setup in Opt's calls (-claude-config): their
// skills, plugins and MCP servers, as Claude Code itself lists them in
// /skills and /mcp, through a control call (see control). A skill or MCP
// server turned on or off here is opt-chat's own choice, kept in its
// settings.json, never in Claude Code's: the user's Claude Code stays as it
// is. Skills' states go to each call as settings (skillOverrides); servers
// are turned off in each call as it starts, since no setting does that for
// all of them (claude.ai's included).

// mcpServer is one MCP server as /mcp shows it.
type mcpServer struct {
	Name   string `json:"name"`
	Status string `json:"status"` // connected, pending, needs-auth, failed, disabled
	Scope  string `json:"scope,omitzero"`
	Source string `json:"source,omitzero"`
	Error  string `json:"error,omitzero"`
	Off    bool   `json:"off,omitzero"` // turned off here
}

// mcpServers lists the user's MCP servers, as the control call sees them:
// connecting as it starts, so a page asks again for the news.
func (a *Agent) mcpServers(ctx context.Context) ([]mcpServer, error) {
	v, err := a.control(ctx, map[string]any{"subtype": "mcp_status"})
	if err != nil {
		return nil, err
	}
	var r struct {
		Servers []mcpServer `json:"mcpServers"` // config left out: it can hold keys
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []mcpServer{}
	for _, s := range r.Servers {
		if s.Name == "opt" { // the chat's own
			continue
		}
		s.Off = slices.Contains(a.mcpOff, s.Name)
		out = append(out, s)
	}
	return out, nil
}

// setMCP turns a server on or off for Opt: kept for later runs, and at
// once in the calls underway.
func (a *Agent) setMCP(ctx context.Context, name string, on bool) error {
	all, err := a.mcpServers(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(all, func(s mcpServer) bool { return s.Name == name }) {
		return fmt.Errorf("no MCP server %q", name)
	}
	a.mu.Lock()
	a.mcpOff = slices.DeleteFunc(a.mcpOff, func(s string) bool { return s == name })
	if !on {
		a.mcpOff = append(a.mcpOff, name)
	}
	off := slices.Clone(a.mcpOff)
	live := make([]*proc, 0, len(a.live))
	for p := range a.live {
		live = append(live, p)
	}
	a.mu.Unlock()
	if err := a.updateSettings(func(s *settings) { s.MCPOff = off }); err != nil {
		return err
	}
	for _, p := range live {
		go mcpSwitch(p, name, on)
	}
	cl, err := a.controller()
	if err != nil {
		return err
	}
	return mcpSwitch(cl.p, name, on)
}

// mcpApply turns off, in a call just started, the servers the user turned
// off, and returns once it has.
func (a *Agent) mcpApply(p *proc) {
	a.mu.Lock()
	off := slices.Clone(a.mcpOff)
	a.mu.Unlock()
	var wg sync.WaitGroup
	for _, name := range off {
		wg.Go(func() { mcpSwitch(p, name, false) })
	}
	wg.Wait()
}

// mcpSettle waits, before a turn or subagent sends its view, until the MCP
// servers connected when the last call sent its view have connected again
// (on a run's first call: until none is pending), and those the user turned
// off are off; for 5 seconds at most. The block Claude Code adds after the
// view lists the servers connected by then: one that connected in time on
// one turn and not on the next changed the prefix, and the view was written
// to the cache again. A server that is always slow (or never answers) is
// not waited for.
//
// Nothing else reads the call's output yet: callNow does.
func (a *Agent) mcpSettle(p *proc) {
	if !a.ownConfig {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	wait := func() bool {
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-time.After(200 * time.Millisecond):
			return true
		case <-p.ctx.Done():
			return false
		}
	}
	a.mu.Lock()
	off, up, seen := slices.Clone(a.mcpOff), a.mcpUp, a.mcpSeen
	a.mu.Unlock()
	var connected, listed []string // as last asked; nil: never answered
	defer func() {
		if listed != nil {
			a.mu.Lock()
			a.mcpUp, a.mcpSeen = connected, listed
			a.mu.Unlock()
		}
	}()
	for {
		v, err := p.callNow(map[string]any{"subtype": "mcp_status"})
		if err != nil {
			return
		}
		var r struct {
			Servers []mcpServer `json:"mcpServers"`
		}
		if json.Unmarshal(v, &r) != nil {
			return
		}
		settled := true
		connected, listed = []string{}, []string{}
		for _, s := range r.Servers {
			listed = append(listed, s.Name)
			if s.Status == "connected" {
				connected = append(connected, s.Name)
			}
			switch {
			case slices.Contains(off, s.Name) && s.Status != "disabled":
				// Listed, maybe not yet ready to be turned off: tried again.
				if _, err := p.callNow(map[string]any{"subtype": "mcp_toggle", "serverName": s.Name, "enabled": false}); err != nil {
					settled = false
				}
			case s.Status == "pending" && (up == nil || slices.Contains(up, s.Name)):
				settled = false
			}
		}
		for _, name := range off { // listed last time, not yet: it would come on after the view went
			if slices.Contains(seen, name) && !slices.Contains(listed, name) {
				settled = false
			}
		}
		if settled || !wait() {
			return
		}
	}
}

// mcpSwitch turns a server on or off in one call. A server Claude Code has
// not listed yet (claude.ai's come a moment after the start) is tried again
// for a while.
func mcpSwitch(p *proc, name string, on bool) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		_, err := p.call(p.ctx, map[string]any{"subtype": "mcp_toggle", "serverName": name, "enabled": on})
		if err == nil || !strings.Contains(err.Error(), "not found") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-p.ctx.Done():
			return p.ctx.Err()
		}
	}
}

// mcpSignIn starts a server's sign-in, and returns the page to open for
// it. The control call waits for the browser's callback.
func (a *Agent) mcpSignIn(ctx context.Context, name string) (string, error) {
	v, err := a.control(ctx, map[string]any{"subtype": "mcp_authenticate", "serverName": name})
	if err != nil {
		return "", err
	}
	var r struct {
		AuthURL string `json:"authUrl"`
	}
	json.Unmarshal(v, &r)
	if r.AuthURL == "" {
		return "", fmt.Errorf("Claude Code gave no sign-in page for %s", name)
	}
	return r.AuthURL, nil
}

func (a *Agent) mcpReconnect(ctx context.Context, name string) error {
	_, err := a.control(ctx, map[string]any{"subtype": "mcp_reconnect", "serverName": name})
	return err
}

// skill is one of the user's skills, as /skills shows it.
type skill struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Tokens      int    `json:"tokens"`
	State       string `json:"state"` // on, name-only, user-invocable-only, off
	LockedBy    string `json:"locked_by,omitzero"`
}

// skills lists the user's skills, in the states set here: the control call
// started with the states of its start.
func (a *Agent) skills(ctx context.Context) ([]skill, error) {
	v, err := a.control(ctx, map[string]any{"subtype": "get_skills_dialog"})
	if err != nil {
		return nil, err
	}
	var r struct {
		Skills []skill `json:"skills"`
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, err
	}
	if r.Skills == nil {
		r.Skills = []skill{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range r.Skills {
		if st, ok := a.skillsSet[s.Name]; ok && (s.LockedBy == "" || s.LockedBy == "flag") {
			r.Skills[i].State, r.Skills[i].LockedBy = st, "" // "flag": locked by our own settings
		}
	}
	return r.Skills, nil
}

var skillStates = []string{"on", "name-only", "user-invocable-only", "off"}

// setSkill sets a skill's state for Opt, from its next call on; the user's
// Claude Code keeps its own.
func (a *Agent) setSkill(ctx context.Context, name, state string) error {
	if !slices.Contains(skillStates, state) {
		return fmt.Errorf("a skill is %s", strings.Join(skillStates, ", "))
	}
	all, err := a.skills(ctx)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(s skill) bool { return s.Name == name })
	switch {
	case i < 0:
		return fmt.Errorf("no skill %q", name)
	case all[i].LockedBy != "":
		return fmt.Errorf("%s is set by %s", name, all[i].LockedBy)
	}
	a.mu.Lock()
	if a.skillsSet == nil {
		a.skillsSet = map[string]string{}
	}
	a.skillsSet[name] = state
	set := maps.Clone(a.skillsSet)
	a.mu.Unlock()
	return a.updateSettings(func(s *settings) { s.Skills = set })
}

// configCommand runs /mcp and /skills in the terminal: a list, or "on
// name" and "off name".
func configCommand(a *Agent, word, arg string) string {
	if !a.ownConfig {
		return "Opt runs without your Claude Code setup (-claude-config=false)."
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	how, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	switch {
	case (how == "on" || how == "off") && name != "" && word == "mcp":
		if err := a.setMCP(ctx, name, how == "on"); err != nil {
			return err.Error()
		}
	case (how == "on" || how == "off") && name != "":
		if err := a.setSkill(ctx, name, how); err != nil {
			return err.Error()
		}
	case arg != "":
		return "/" + word + " lists them; /" + word + " on NAME or off NAME turns one on or off."
	}
	var lines []string
	if word == "mcp" {
		ss, err := a.mcpServers(ctx)
		if err != nil {
			return err.Error()
		}
		for _, s := range ss {
			st := s.Status
			if s.Off {
				st = "off"
			}
			lines = append(lines, fmt.Sprintf("%-40s %s", s.Name, st))
		}
	} else {
		ss, err := a.skills(ctx)
		if err != nil {
			return err.Error()
		}
		for _, s := range ss {
			lines = append(lines, fmt.Sprintf("%-40s %-4s %s", s.Name, s.State, s.Source))
		}
	}
	if len(lines) == 0 {
		return "None."
	}
	return strings.Join(lines, "\n")
}
