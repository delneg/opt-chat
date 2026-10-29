package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"time"
)

// /status, after Claude Code's
// (https://code.claude.com/docs/en/slash-commands): Claude Code's version
// and account, which its initialize reply carries, then what this run is
// set to and where the chat stands.

// ccInfo is what /status takes from Claude Code's initialize reply.
type ccInfo struct {
	Version string `json:"claude_code_version"`
	Account struct {
		Email        string `json:"email"`
		Subscription string `json:"subscriptionType"`
		Provider     string `json:"apiProvider"`
	} `json:"account"`
}

// statusCommand is /status's answer. Claude Code is asked for its version
// and the plan's usage; when it can't answer, the rest still comes.
func statusCommand(a *Agent) string {
	ctx, cancel := context.WithTimeout(a.life, 30*time.Second)
	defer cancel()
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	var cc ccInfo
	v, err := a.control(ctx, map[string]any{"subtype": "initialize"})
	if err == nil {
		err = json.Unmarshal(v, &cc)
	}
	switch acc := cc.Account; {
	case err != nil:
		add("Claude Code: %v", err)
	case acc.Email != "":
		add("Claude Code %s, signed in as %s (%s, %s)", cc.Version, acc.Email, cmp.Or(acc.Subscription, "no plan"), acc.Provider)
	default:
		add("Claude Code %s, %s", cc.Version, cmp.Or(acc.Provider, "not signed in"))
	}
	usageErr := a.askUsage(ctx)

	c := a.chat
	s := currentStatus(c, a)
	add("Chat: %s, %s; Opt's tools run in %s", s.Dir, count(s.Msgs, "message"), s.Cwd)
	view := fmt.Sprintf("View: %s, %.1f KB (merged down to %d KB past %d KB)", count(s.ViewLines, "line"), float64(s.ViewBytes)/1000, viewLo/1000, viewHi/1000)
	var work []string
	if s.Backlog > 0 {
		work = append(work, count(s.Backlog, "message")+" to summarize")
	}
	if s.Merges > 0 {
		work = append(work, count(s.Merges, "merge")+" to build")
	}
	if s.Running > 0 {
		work = append(work, count(s.Running, "compaction")+" running")
	}
	if s.Failing > 0 {
		work = append(work, fmt.Sprintf("%d failed, to try again", s.Failing))
	}
	if len(work) > 0 {
		view += "; " + strings.Join(work, ", ")
	}
	add("%s", view)

	a.mu.Lock()
	recall := "off"
	if a.recall > 0 {
		recall = count(a.recall, "message")
	}
	add("Turns: %s, effort %s, mode %s; recall %s", s.Model, s.Effort, s.Mode, recall)
	auto := "off"
	if w := a.agentWindowLocked(a.agentWindow); w > 0 {
		auto = fmt.Sprintf("past %dk tokens", w/1000)
	}
	subs := fmt.Sprintf("Subagents: %s, effort %s; autocompact %s", a.amodel, a.aeffort, auto)
	if len(s.Agents) > 0 {
		subs += "; at work: [" + strings.Join(s.Agents, "], [") + "]"
	}
	if resting := sortedKeys(a.resting); len(resting) > 0 {
		subs += "; reported: [" + strings.Join(resting, "], [") + "]"
	}
	add("%s", subs)
	add("Compactions: %s, effort %s", a.cmodel, a.ceffort)
	up, seen, own := a.mcpUp, a.mcpSeen, a.ownConfig
	var forAgents []string
	for _, name := range a.mcpAgents {
		if g := a.grantedLocked(name); len(g) > 0 {
			name += " (granted to [" + strings.Join(g, "], [") + "])"
		}
		forAgents = append(forAgents, name)
	}
	a.mu.Unlock()

	if e := c.hub.lastEnd(); e != nil {
		in := e.Turn.Input + e.Turn.Write + e.Turn.Read
		line := fmt.Sprintf("Last turn, %s: context %.1fk; %s", time.UnixMilli(e.At).Format("Jan 2 15:04"), float64(e.Context)/1000, e.Turn)
		if in > 0 {
			line += fmt.Sprintf("; %d%% of input read from the cache", e.Turn.Read*100/in)
		}
		add("%s", line)
	}
	if len(s.Limits) > 0 {
		var ws []string
		for _, w := range s.Limits {
			x := fmt.Sprintf("%s %.0f%%", strings.ReplaceAll(w.Name, "_", "-"), w.Used)
			if w.Limit > 0 {
				x += fmt.Sprintf(" ($%.0f of $%.0f)", w.Spent, w.Limit)
			}
			if w.Resets > 0 {
				x += ", resets " + time.Unix(w.Resets, 0).Format("Jan 2 15:04")
			}
			ws = append(ws, x)
		}
		add("Usage: %s", strings.Join(ws, "; "))
	} else if usageErr != nil {
		add("Usage: %v", usageErr)
	}
	if own && seen != nil {
		var off []string
		for _, name := range seen {
			if !slices.Contains(up, name) {
				off = append(off, name)
			}
		}
		line := "MCP servers at the last call: " + cmp.Or(strings.Join(up, ", "), "none") + " connected"
		if len(off) > 0 {
			line += "; not: " + strings.Join(off, ", ")
		}
		add("%s", line)
	}
	if own && len(forAgents) > 0 {
		add("MCP servers for subagents only: %s", strings.Join(forAgents, ", "))
	}
	return strings.Join(lines, "\n")
}
