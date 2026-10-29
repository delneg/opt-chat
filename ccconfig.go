package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// The user's own Claude Code setup in Opt's calls (-claude-config): their
// skills, plugins and MCP servers, as Claude Code itself lists them in
// /skills and /mcp, through a control call (see control). A skill or MCP
// server turned on or off here is opt-chat's own choice, kept in its
// settings.json, never in Claude Code's: the user's Claude Code stays as it
// is. Skills' states go to each call as settings (skillOverrides), and so
// do the servers kept out of it (deniedMcpServers, claude.ai's included):
// see ownSettings.
//
// A server can also be for subagents only: off in Opt's calls, so its tools
// and instructions are not in Opt's prompt, and on in a subagent's call when
// Opt grants it, by spawn's or tell's mcp.

// mcpServer is one MCP server as /mcp shows it.
type mcpServer struct {
	Name   string `json:"name"`
	Status string `json:"status"` // connected, pending, needs-auth, failed, disabled
	Scope  string `json:"scope,omitzero"`
	Source string `json:"source,omitzero"`
	Error  string `json:"error,omitzero"`
	Off    bool   `json:"off,omitzero"`    // turned off here
	Agents bool   `json:"agents,omitzero"` // for subagents only
	// Granted is the subagents at work or at rest that Opt granted it to.
	Granted []string `json:"granted,omitzero"`
	// Its tools, and what they and its instructions take of the context,
	// as Claude Code's /context counts them: the tools only once used, with
	// tool search.
	ToolCount int `json:"toolCount,omitzero"` // not "tools": Claude Code's status has its own
	Tokens    int `json:"tokens,omitzero"`
}

// mcpCounts is the servers' tools and tokens, by mcpToolName, for the
// servers connected then (key), in a context window of window tokens.
type mcpCounts struct {
	key    string
	window int
	tools  map[string]int
	tokens map[string]int
}

// mcpToolName is the name Claude Code makes of a server's for its tools:
// what isn't a letter, digit, _ or - made _.
func mcpToolName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, name)
}

// countMCP asks the control call for what each server takes of the
// context, unless the servers connected are those counted last.
func (a *Agent) countMCP(ctx context.Context, servers []mcpServer) {
	var up []string
	for _, s := range servers {
		if s.Status == "connected" {
			up = append(up, s.Name)
		}
	}
	key := strings.Join(up, "\x00")
	a.mu.Lock()
	same := a.mcpCounted.key == key && a.mcpCounted.tokens != nil
	a.mu.Unlock()
	if same {
		return
	}
	c := mcpCounts{key: key, tools: map[string]int{}, tokens: map[string]int{}}
	defer func() { // a failure too, so a page's every ask doesn't ask again
		a.mu.Lock()
		a.mcpCounted = c
		a.mu.Unlock()
	}()
	v, err := a.control(ctx, map[string]any{"subtype": "get_context_usage"})
	if err != nil {
		return // the list, without them
	}
	var r struct {
		Window int `json:"maxTokens"`
		Tools  []struct {
			Server string `json:"serverName"`
			Tokens int    `json:"tokens"`
		} `json:"mcpTools"`
		Instructions []struct {
			Server string `json:"serverName"`
			Tokens int    `json:"tokens"`
		} `json:"mcpInstructions"`
	}
	if json.Unmarshal(v, &r) != nil {
		return
	}
	c.window = r.Window
	for _, t := range r.Tools {
		c.tools[mcpToolName(t.Server)]++
		c.tokens[mcpToolName(t.Server)] += t.Tokens
	}
	for _, in := range r.Instructions {
		c.tokens[mcpToolName(in.Server)] += in.Tokens
	}
}

// mcpWindowNow is the context window the servers' tokens were counted in;
// 0: not counted.
func (a *Agent) mcpWindowNow() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mcpCounted.window
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
	a.countMCP(ctx, r.Servers)
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []mcpServer{}
	for _, s := range r.Servers {
		if s.Name == "opt" { // the chat's own
			continue
		}
		s.Off, s.Agents = slices.Contains(a.mcpOff, s.Name), slices.Contains(a.mcpAgents, s.Name)
		s.ToolCount, s.Tokens = a.mcpCounted.tools[mcpToolName(s.Name)], a.mcpCounted.tokens[mcpToolName(s.Name)]
		if s.Agents {
			s.Granted = a.grantedLocked(s.Name)
		}
		out = append(out, s)
	}
	return out, nil
}

// setMCP sets a server on, off, or for subagents only ("agents"): kept for
// later runs, from each call's start (see ownSettings): Opt's next turn, a
// subagent's next call. A subagent keeps a server Opt granted it while the
// server is not off.
func (a *Agent) setMCP(ctx context.Context, name, state string) error {
	if state != "on" && state != "off" && state != "agents" {
		return fmt.Errorf("an MCP server is on, off or agents (for subagents only), not %q", state)
	}
	all, err := a.mcpServers(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(all, func(s mcpServer) bool { return s.Name == name }) {
		return fmt.Errorf("no MCP server %q", name)
	}
	a.mu.Lock()
	other := func(s string) bool { return s == name }
	a.mcpOff, a.mcpAgents = slices.DeleteFunc(a.mcpOff, other), slices.DeleteFunc(a.mcpAgents, other)
	switch state {
	case "off":
		a.mcpOff = append(a.mcpOff, name)
	case "agents":
		a.mcpAgents = append(a.mcpAgents, name)
		slices.Sort(a.mcpAgents) // as spawn's description lists them: the same bytes each turn
	}
	off, agents := slices.Clone(a.mcpOff), slices.Clone(a.mcpAgents)
	a.mu.Unlock()
	return a.updateSettings(func(s *settings) { s.MCPOff, s.MCPAgents = off, agents })
}

// mcpOffLocked is the servers off in a call of who, a subagent, or "" for
// Opt's: those turned off, and those for subagents only but those Opt
// granted who. a.mu held.
func (a *Agent) mcpOffLocked(who string) []string {
	off := slices.Clone(a.mcpOff)
	var granted []string
	if s := a.subs[who]; who != "" && s != nil {
		granted = s.MCP
	}
	for _, name := range a.mcpAgents {
		if !slices.Contains(granted, name) {
			off = append(off, name)
		}
	}
	return off
}

// grantedLocked is the subagents at work or at rest that have server, for
// subagents only. a.mu held.
func (a *Agent) grantedLocked(server string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(a.subs)) {
		_, working := a.running[name]
		_, resting := a.resting[name]
		if (working || resting) && slices.Contains(a.subs[name].MCP, server) {
			out = append(out, name)
		}
	}
	return out
}

// checkMCP reads the servers Opt asks to grant a subagent: those for
// subagents only it gets; one on for all it has already. Any other is
// refused, with why and the servers there are to grant.
func (a *Agent) checkMCP(servers []string) ([]string, string) {
	if len(servers) == 0 {
		return nil, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ownConfig {
		return nil, "Opt runs without the user's Claude Code setup (-claude-config=false): no MCP servers to grant."
	}
	var grant []string
	for _, s := range servers {
		why := ""
		switch {
		case slices.Contains(a.mcpOff, s):
			why = s + " is off: the user turned it off in /mcp."
		case slices.Contains(a.mcpAgents, s):
			if !slices.Contains(grant, s) {
				grant = append(grant, s)
			}
			continue
		case slices.Contains(a.mcpSeen, s):
			continue // on for all
		default:
			why = "no MCP server " + s + "."
		}
		return nil, why + " Servers for subagents only: " + cmp.Or(strings.Join(a.mcpAgents, ", "), "none") + "."
	}
	return grant, ""
}

// grantLocked gives the subagent name servers for subagents only, kept in
// its record: its next call has them (see restartLocked for one underway).
// a.mu held.
func (a *Agent) grantLocked(name string, servers []string) {
	add := func(have []string) []string {
		for _, s := range servers {
			if !slices.Contains(have, s) {
				have = append(have, s)
			}
		}
		return have
	}
	if _, err := a.loadRecord(name); err != nil {
		return
	}
	a.note(name, func(r *agentRecord) { r.MCP = add(r.MCP) })
	if s := a.subs[name]; s != nil {
		s.MCP = add(s.MCP)
	}
}

// mcpSettle waits, before a turn or subagent sends its view, until the MCP
// servers connected when the last call sent its view have connected again
// (on a run's first call: until none is pending); for 5 seconds at most.
// Those kept out of the call (see ownSettings) are never listed. The block Claude Code adds after the
// view lists the servers connected by then: one that connected in time on
// one turn and not on the next changed the prefix, and the view was written
// to the cache again. A server that is always slow (or never answers) is
// not waited for.
//
// A subagent's call also waits for the servers Opt granted it, and gets
// their statuses as last asked, to say which are not there.
//
// Nothing else reads the call's output yet: callNow does.
func (a *Agent) mcpSettle(p *proc) (servers []mcpServer, granted []string) {
	if !a.ownConfig {
		return nil, nil
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
	off, up := a.mcpOffLocked(p.who), a.mcpUp
	if s := a.subs[p.who]; p.who != "" && s != nil {
		granted = slices.DeleteFunc(slices.Clone(s.MCP), func(n string) bool { return slices.Contains(off, n) })
	}
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
			return servers, granted
		}
		var r struct {
			Servers []mcpServer `json:"mcpServers"`
		}
		if json.Unmarshal(v, &r) != nil {
			return servers, granted
		}
		servers = r.Servers
		settled := true
		connected, listed = []string{}, []string{}
		for _, s := range r.Servers {
			listed = append(listed, s.Name)
			if s.Status == "connected" {
				connected = append(connected, s.Name)
			}
			if s.Status == "pending" && (up == nil || slices.Contains(up, s.Name) || slices.Contains(granted, s.Name)) {
				settled = false
			}
		}
		if settled || !wait() {
			return servers, granted
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

// mcpReconnect reconnects a server in the page's call, and in each call
// underway that has it on: a subagent's that failed goes on with it.
func (a *Agent) mcpReconnect(ctx context.Context, name string) error {
	a.mu.Lock()
	var live []*proc
	for p := range a.live {
		if !slices.Contains(a.mcpOffLocked(p.who), name) {
			live = append(live, p)
		}
	}
	a.mu.Unlock()
	for _, p := range live {
		go p.call(p.ctx, map[string]any{"subtype": "mcp_reconnect", "serverName": name})
	}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	a.skillsListed = r.Skills
	return a.skillsHere(), nil
}

// skillsHere are the skills listed last, in the states set here. Under a.mu.
func (a *Agent) skillsHere() []skill {
	ss := append([]skill{}, a.skillsListed...)
	for i, s := range ss {
		if st, ok := a.skillsSet[s.Name]; ok && (s.LockedBy == "" || s.LockedBy == "flag") {
			ss[i].State, ss[i].LockedBy = st, "" // "flag": locked by our own settings
		}
	}
	return ss
}

// skillState is the state for Opt of the user's skill so named, "" for no
// skill (or none to be had: without the user's setup, Claude Code takes no
// /name). One not listed last is asked of Claude Code, unless fast.
func (a *Agent) skillState(name string, fast bool) (string, bool) {
	if !a.ownConfig {
		return "", true
	}
	find := func(ss []skill) (string, bool) {
		i := slices.IndexFunc(ss, func(s skill) bool { return s.Name == name })
		if i < 0 {
			return "", false
		}
		return ss[i].State, true
	}
	a.mu.Lock()
	st, ok := find(a.skillsHere())
	a.mu.Unlock()
	if ok || fast {
		return st, ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ss, _ := a.skills(ctx)
	st, _ = find(ss)
	return st, true
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

// plugin is one of the user's Claude Code plugins, as /plugins shows it:
// with the skills and MCP servers it brings, while on.
type plugin struct {
	ID     string `json:"id"` // name@marketplace
	Name   string `json:"name"`
	Scope  string `json:"scope"` // user, project, local
	On     bool   `json:"on"`
	Skills int    `json:"skills"`
	Tokens int    `json:"tokens"` // its skills' listing, every turn
	MCP    int    `json:"mcp"`    // its MCP servers
}

// plugins lists the user's plugins, on or off as set here, else as
// Claude Code in -cwd has them.
func (a *Agent) plugins(ctx context.Context) ([]plugin, error) {
	cmd := exec.CommandContext(ctx, a.claudeCmd(), "plugin", "list", "--json")
	os.MkdirAll(a.cwd, 0o700) // failing, the command says so
	cmd.Dir, cmd.Env = a.cwd, childEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude plugin list: %v", err)
	}
	var all []struct {
		ID      string `json:"id"`
		Scope   string `json:"scope"`
		Enabled bool   `json:"enabled"`        // installed for the user: on everywhere
		Here    bool   `json:"projectEnabled"` // installed for a folder: on in -cwd
	}
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, err
	}
	skills, _ := a.skills(ctx) // their costs, for those on
	servers, _ := a.mcpServers(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	ps := []plugin{}
	for _, e := range all {
		if e.Scope != "user" {
			e.Enabled = e.Here
		}
		if i := slices.IndexFunc(ps, func(p plugin) bool { return p.ID == e.ID }); i >= 0 { // installed for several folders
			ps[i].On = ps[i].On || e.Enabled
			continue
		}
		name, _, _ := strings.Cut(e.ID, "@")
		ps = append(ps, plugin{ID: e.ID, Name: name, Scope: e.Scope, On: e.Enabled})
	}
	for i := range ps {
		p := &ps[i]
		if on, ok := a.pluginsSet[p.ID]; ok {
			p.On = on
		}
		if !p.On { // nothing of it is loaded
			continue
		}
		for _, sk := range skills {
			if strings.HasPrefix(sk.Name, p.Name+":") {
				p.Skills++
				p.Tokens += sk.Tokens
			}
		}
		for _, sv := range servers {
			if strings.HasPrefix(sv.Name, "plugin:"+p.Name+":") {
				p.MCP++
			}
		}
	}
	return ps, nil
}

// setPlugin turns a plugin on or off for Opt, from its next call on; the
// user's Claude Code keeps its own. The control call restarts with it, so
// /skills and /mcp list what Opt's calls get.
func (a *Agent) setPlugin(ctx context.Context, id string, on bool) error {
	all, err := a.plugins(ctx)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(all, func(p plugin) bool { return p.ID == id }) {
		return fmt.Errorf("no plugin %q", id)
	}
	a.mu.Lock()
	if a.pluginsSet == nil {
		a.pluginsSet = map[string]bool{}
	}
	a.pluginsSet[id] = on
	set := maps.Clone(a.pluginsSet)
	a.mu.Unlock()
	if err := a.updateSettings(func(s *settings) { s.Plugins = set }); err != nil {
		return err
	}
	a.ctlMu.Lock()
	cl := a.ctl
	a.ctl = nil
	a.ctlMu.Unlock()
	if cl != nil {
		cl.idle.Stop()
		cl.p.close()
	}
	return nil
}

// configCommand runs /mcp, /skills and /plugins in the terminal: a list,
// or "on name" and "off name"; for an MCP server, "agents name" too.
func configCommand(a *Agent, word, arg string) string {
	if !a.ownConfig {
		return "Opt runs without your Claude Code setup (-claude-config=false)."
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	how, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	switch {
	case (how == "on" || how == "off" || how == "agents") && name != "" && word == "mcp":
		if err := a.setMCP(ctx, name, how); err != nil {
			return err.Error()
		}
	case (how == "on" || how == "off") && name != "" && word == "plugins":
		if err := a.setPlugin(ctx, name, how == "on"); err != nil {
			return err.Error()
		}
	case (how == "on" || how == "off") && name != "":
		if err := a.setSkill(ctx, name, how); err != nil {
			return err.Error()
		}
	case arg != "" && word == "mcp":
		return "/mcp lists them; /mcp on NAME, off NAME or agents NAME sets one on, off or for subagents only."
	case arg != "":
		return "/" + word + " lists them; /" + word + " on NAME or off NAME turns one on or off."
	}
	var lines []string
	switch word {
	case "plugins":
		ps, err := a.plugins(ctx)
		if err != nil {
			return err.Error()
		}
		for _, p := range ps {
			st := "off"
			if p.On {
				st = fmt.Sprintf("on   %d skills, %d tokens, %d MCP servers", p.Skills, p.Tokens, p.MCP)
			}
			lines = append(lines, fmt.Sprintf("%-50s %s", p.ID, st))
		}
	case "mcp":
		ss, err := a.mcpServers(ctx)
		if err != nil {
			return err.Error()
		}
		w := a.mcpWindowNow()
		for _, s := range ss {
			st := s.Status
			switch {
			case s.Off:
				st = "off"
			case s.Agents:
				st = "for subagents only, " + st
				if len(s.Granted) > 0 {
					st += ", granted to [" + strings.Join(s.Granted, "], [") + "]"
				}
			}
			if s.Tokens > 0 && w > 0 {
				st += fmt.Sprintf("   %s, %d tokens (%.1f%%)", count(s.ToolCount, "tool"), s.Tokens, 100*float64(s.Tokens)/float64(w))
			}
			lines = append(lines, fmt.Sprintf("%-40s %s", s.Name, st))
		}
	default:
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
