package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// /usage, after Claude Code's (https://code.claude.com/docs/en/costs): the
// plan's limits and credits, and what is using them on this machine, as
// get_usage tells them; and what opt-chat's own calls spent, which every
// call's results tell (total_cost_usd, modelUsage) and usage/ keeps, by day.

// modelSpend is one model's part of a call's spend, as a result's
// modelUsage tells it.
type modelSpend struct {
	Input    int     `json:"inputTokens"`
	Output   int     `json:"outputTokens"`
	Read     int     `json:"cacheReadInputTokens"`
	Write    int     `json:"cacheCreationInputTokens"`
	Searches int     `json:"webSearchRequests,omitzero"`
	Cost     float64 `json:"costUSD"` // at list prices
}

func (m modelSpend) add(o modelSpend, sign int) modelSpend {
	return modelSpend{m.Input + sign*o.Input, m.Output + sign*o.Output, m.Read + sign*o.Read, m.Write + sign*o.Write,
		m.Searches + sign*o.Searches, m.Cost + float64(sign)*o.Cost}
}

// spent is a call's spend as its last result told it. Claude Code counts
// from the session's start, so a resumed session's spend starts from its
// last run's.
type spent struct {
	Cost   float64
	APIms  float64
	Models map[string]modelSpend
}

// spend is what one result of a call cost, as kept in usage/.
type spend struct {
	Date     time.Time             `json:"date"`
	Role     string                `json:"role"`         // turn, agent, compaction or btw
	Who      string                `json:"who,omitzero"` // a subagent's name
	Requests int                   `json:"requests"`
	APIms    int64                 `json:"apiMs"`
	Cost     float64               `json:"cost"`
	Models   map[string]modelSpend `json:"models"`
}

var roleNames = map[role]string{roleTurn: "turn", roleAgent: "agent", roleCompact: "compaction", roleBtw: "btw"}

// started is when this run began, for /usage run.
var started = time.Now()

// noteSpend keeps what p's call spent since its last result. Only the
// call's reader calls it, so p.spent needs no lock.
func (a *Agent) noteSpend(p *proc, r role, e event) {
	if e.Models == nil { // no totals: nothing to count from
		return
	}
	s := spend{Date: time.Now(), Role: roleNames[r], Who: p.who, Requests: e.Turns,
		APIms: int64(e.APIms - p.spent.APIms), Cost: e.Cost - p.spent.Cost, Models: map[string]modelSpend{}}
	for name, m := range e.Models {
		if d := m.add(p.spent.Models[name], -1); d != (modelSpend{}) {
			s.Models[name] = d
		}
	}
	p.spent = spent{Cost: e.Cost, APIms: e.APIms, Models: e.Models}
	if len(s.Models) == 0 {
		return
	}
	b, err := json.Marshal(s)
	if err == nil {
		err = appendRaw(filepath.Join(a.chat.dir, "usage", s.Date.Format(time.DateOnly)+".jsonl"), append(b, '\n'))
	}
	if err != nil {
		dim("usage: " + err.Error())
	}
}

// resumedSpend is what a resumed session spent before this call, which its
// results count in: get_usage tells it. p's output is not read yet.
func resumedSpend(p *proc) spent {
	var r struct {
		Session struct {
			Cost   float64               `json:"total_cost_usd"`
			APIms  float64               `json:"total_api_duration_ms"`
			Models map[string]modelSpend `json:"model_usage"`
		} `json:"session"`
	}
	v, err := p.callNow(map[string]any{"subtype": "get_usage"})
	if err == nil {
		err = json.Unmarshal(v, &r)
	}
	if err != nil {
		dim("usage: a resumed call's spend so far is unknown, so it counts again: " + err.Error())
		return spent{}
	}
	return spent{r.Session.Cost, r.Session.APIms, r.Session.Models}
}

// readSpend is what usage/ holds from since on.
func readSpend(dir string, since time.Time) ([]spend, error) {
	files, err := filepath.Glob(filepath.Join(dir, "usage", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	first := since.AddDate(0, 0, -1).Format(time.DateOnly) // a day file is named by the clock, which can go back
	var out []spend
	for _, f := range files {
		if strings.TrimSuffix(filepath.Base(f), ".jsonl") < first {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		for line := range bytes.SplitSeq(b, []byte("\n")) {
			var s spend
			if json.Unmarshal(line, &s) == nil && !s.Date.Before(since) {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// planUsage is what /usage takes from get_usage, beyond the windows the bar
// shows (see askUsage).
type planUsage struct {
	Subscription string `json:"subscription_type"`
	Limits       struct {
		Limits []struct {
			Kind     string  `json:"kind"` // session, weekly_all, weekly_scoped, ...
			Percent  float64 `json:"percent"`
			ResetsAt string  `json:"resets_at"`
			Scope    *struct {
				Model *struct {
					Name string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
		Spend *struct {
			Used    money `json:"used"`
			Limit   money `json:"limit"`
			Enabled bool  `json:"enabled"`
		} `json:"spend"`
		Breakdown *struct {
			Rows []struct {
				Name    string  `json:"display_name"`
				Percent float64 `json:"percent"`
			} `json:"rows"`
		} `json:"seven_day_breakdown"`
	} `json:"rate_limits"`
	Behaviors map[string]struct { // day, week
		Requests  int     `json:"request_count"`
		Sessions  int     `json:"session_count"`
		Behaviors []share `json:"behaviors"`
		Agents    []share `json:"agents"`
		Skills    []share `json:"skills"`
		Plugins   []share `json:"plugins"`
		MCP       []share `json:"mcp_servers"`
	} `json:"behaviors"`
}

type money struct {
	Minor    int64 `json:"amount_minor"`
	Exponent int   `json:"exponent"`
}

func (m money) String() string {
	f := float64(m.Minor)
	for range m.Exponent {
		f /= 10
	}
	return fmt.Sprintf("$%.2f", f)
}

type share struct {
	Key  string  `json:"key"`
	Name string  `json:"name"`
	Pct  float64 `json:"pct"`
}

// usageCommand is /usage's answer, for period: day (since midnight), week
// (the last seven days) or run.
func usageCommand(a *Agent, period string) (string, error) {
	now := time.Now()
	var since time.Time
	var title string
	switch period {
	case "", "day":
		period, since, title = "day", time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local), "today"
	case "week":
		since, title = now.AddDate(0, 0, -7), "in the last 7 days"
	case "run":
		period, since, title = "day", started, "since this run began, "+when(started, now)
	default:
		return "", fmt.Errorf("/usage takes day, week or run")
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	ctx, cancel := context.WithTimeout(a.life, 30*time.Second)
	defer cancel()
	a.mu.Lock()
	a.usageAsked = time.Time{} // fresh: the bar's minute is for the bar
	a.mu.Unlock()
	if err := a.askUsage(ctx); err != nil {
		line("The plan: %v", err)
	}
	a.mu.Lock()
	raw := a.planRaw
	a.mu.Unlock()
	var plan planUsage
	if raw != nil && json.Unmarshal(raw, &plan) == nil {
		planLines(&b, plan, period, now)
	}

	all, err := readSpend(a.chat.dir, since)
	if err != nil {
		return "", err
	}
	spendLines(&b, all, title)
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func planLines(b *strings.Builder, plan planUsage, period string, now time.Time) {
	line := func(format string, args ...any) { fmt.Fprintf(b, format+"\n", args...) }
	lim := plan.Limits
	if len(lim.Limits) > 0 || lim.Spend != nil {
		line("Your plan (%s):", cmp.Or(plan.Subscription, "no plan"))
	}
	for _, l := range lim.Limits {
		name := map[string]string{"session": "Current session", "weekly_all": "Current week (all models)"}[l.Kind]
		if name == "" && l.Scope != nil && l.Scope.Model != nil {
			name = "Current week (" + l.Scope.Model.Name + ")"
		}
		name = cmp.Or(name, strings.ReplaceAll(l.Kind, "_", " "))
		s := fmt.Sprintf("  %-27s %s %3.0f%% used", name, bar(l.Percent), l.Percent)
		if t, err := time.Parse(time.RFC3339Nano, l.ResetsAt); err == nil {
			s += ", resets " + when(t, now)
		}
		line("%s", s)
	}
	if sp := lim.Spend; sp != nil && sp.Enabled {
		line("  %-27s %s of %s spent", "Usage credits", sp.Used, sp.Limit)
	}
	if bd := lim.Breakdown; bd != nil && len(bd.Rows) > 0 {
		var parts []string
		for _, r := range bd.Rows {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", r.Name, r.Percent))
		}
		line("  The week by where it was used: %s", strings.Join(parts, ", "))
	}
	if bh, ok := plan.Behaviors[period]; ok && bh.Requests > 0 {
		line("")
		line("All of Claude Code on this machine, last %s (%s, %s):", map[string]string{"day": "24h", "week": "7 days"}[period],
			count(bh.Requests, "request"), count(bh.Sessions, "session"))
		names := map[string]string{"long_context": "long context", "subagent_heavy": "subagent-heavy", "cron": "loops"}
		for _, l := range []struct {
			name string
			of   []share
		}{{"Usage came from", bh.Behaviors}, {"Subagents", bh.Agents}, {"Skills", bh.Skills}, {"Plugins", bh.Plugins}, {"MCP servers", bh.MCP}} {
			if len(l.of) == 0 {
				continue
			}
			var parts []string
			for _, s := range l.of {
				parts = append(parts, fmt.Sprintf("%s %.0f%%", cmp.Or(names[s.Key], s.Name, s.Key), s.Pct))
			}
			line("  %-15s %s", l.name+":", strings.Join(parts, ", "))
		}
		if len(bh.Behaviors) > 0 {
			line("  (each a share of all usage, not a breakdown)")
		}
	}
}

func spendLines(b *strings.Builder, all []spend, title string) {
	line := func(format string, args ...any) { fmt.Fprintf(b, format+"\n", args...) }
	if b.Len() > 0 {
		line("")
	}
	if len(all) == 0 {
		line("opt-chat's calls %s: none recorded.", title)
		return
	}
	var total float64
	var api int64
	var requests int
	models := map[string]modelSpend{}
	byRole := map[string]float64{}
	byAgent := map[string]float64{}
	for _, s := range all {
		total, api, requests = total+s.Cost, api+s.APIms, requests+s.Requests
		byRole[s.Role] += s.Cost
		if s.Role == "agent" {
			byAgent[s.Who] += s.Cost
		}
		for name, m := range s.Models {
			models[name] = models[name].add(m, 1)
		}
	}
	var read, in int
	for _, m := range models {
		read, in = read+m.Read, in+m.Input+m.Read+m.Write
	}
	cached := ""
	if in > 0 {
		cached = fmt.Sprintf("; %.0f%% of input read from the cache", float64(read)*100/float64(in))
	}
	line("opt-chat's calls %s: $%.2f at API list prices, %s, %s of API time%s", title, total, count(requests, "request"),
		(time.Duration(api) * time.Millisecond).Round(time.Second), cached)
	names := slices.SortedFunc(maps.Keys(models), func(x, y string) int { return cmp.Compare(models[y].Cost, models[x].Cost) })
	for _, name := range names {
		m := models[name]
		s := fmt.Sprintf("  %-20s %s in, %s out, %s cache read, %s cache write", name+":", tokens(m.Input), tokens(m.Output), tokens(m.Read), tokens(m.Write))
		if m.Searches > 0 {
			s += ", " + count(m.Searches, "web search")
		}
		line("%s ($%.2f)", s, m.Cost)
	}
	pct := func(c float64) string {
		if total <= 0 {
			return ""
		}
		return fmt.Sprintf(" (%.0f%%)", c*100/total)
	}
	for _, r := range []struct{ role, name string }{{"turn", "The chat's turns"}, {"agent", "Subagents"}, {"compaction", "Compactions"}, {"btw", "Side questions"}} {
		c, ok := byRole[r.role]
		if !ok {
			continue
		}
		s := fmt.Sprintf("  %-20s $%.2f%s", r.name+":", c, pct(c))
		if r.role == "agent" {
			var parts []string
			for _, who := range slices.SortedFunc(maps.Keys(byAgent), func(x, y string) int { return cmp.Compare(byAgent[y], byAgent[x]) }) {
				parts = append(parts, fmt.Sprintf("[%s] $%.2f", who, byAgent[who]))
			}
			s += ": " + strings.Join(parts, ", ")
		}
		line("%s", s)
	}
}

// bar is a percent as 20 cells.
func bar(pct float64) string {
	n := min(max(int(pct/5+0.5), 0), 20)
	return strings.Repeat("█", n) + strings.Repeat("░", 20-n)
}

// tokens is a count of tokens, as 1.5m or 409.7k.
func tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fm", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

// when is t as the clock shows it, with its day unless it is today.
func when(t, now time.Time) string {
	t = t.Local()
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
