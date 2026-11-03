package main

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// Opt may let a subagent start and tell subagents of its own (spawn's
// spawn), as Claude Code's own agents can't: a task big enough to split
// among helpers. Each subagent's calls reach the MCP server at an address
// of their own (agentMCPFor), so the server knows who calls. A subagent's
// own report to it as a tell, which sets it to work again if it rested,
// and its own can't start any: one level only. Opt's chat notes each one
// started, and the rail lists them, under their parent.

// agentMCPFor is the subagents' --mcp-config cfg, for the subagent who.
func agentMCPFor(cfg, who string) string {
	return strings.Replace(cfg, `?agent"`, `?agent=`+url.QueryEscape(who)+`"`, 1)
}

const notLet = "Only Opt starts and tells subagents, and only those it lets as it starts them (spawn's spawn) start their own."

// subCall runs spawn or tell for the subagent caller.
func (a *Agent) subCall(caller, tool string, x toolArgs) string {
	rec, err := a.loadRecord(caller)
	if caller == "" || err != nil || !rec.Spawns {
		return notLet
	}
	if i := slices.IndexFunc(x.MCP, func(s string) bool { return !slices.Contains(rec.MCP, s) }); i >= 0 {
		return fmt.Sprintf("You can give yours only the servers Opt granted you, not %s.", x.MCP[i])
	}
	if tool == "spawn" {
		if x.Spawns {
			return "Your subagents can't start their own."
		}
		return a.spawn(caller, false, x.Name, x.Task, x.Effort, x.Model, x.Window, x.Loop, x.MCP...)
	}
	name := agentName(x.Name)
	if r, err := a.loadRecord(name); err != nil || r.Parent != caller {
		return fmt.Sprintf("You tell only the subagents you started; [%s] isn't one.", name)
	}
	grant, bad := a.checkMCP(x.MCP)
	if bad != "" {
		return bad
	}
	out, _ := a.tell(name, tell{text: fmt.Sprintf("From [%s], the subagent that started you:\n%s", caller, x.Message), from: caller, grant: grant})
	return out
}

// toParent hands the report text of the subagent name to its parent, as
// a tell; false when the user stopped the parent, or it can't take one:
// the report goes to Opt then.
func (a *Agent) toParent(parent, name, text string) bool {
	if r, err := a.loadRecord(parent); err != nil || r.State == "halted" {
		return false
	}
	_, ok := a.tell(parent, tell{text: fmt.Sprintf("From [%s], your subagent:\n%s", name, text), from: name})
	return ok
}
