package main

import (
	"context"
	"encoding/json/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// denied is the MCP servers a call's settings keep out of it.
func denied(args []string) []string {
	var s struct {
		Denied []struct {
			Name string `json:"serverName"`
		} `json:"deniedMcpServers"`
	}
	json.Unmarshal([]byte(flagOf(args, "--settings")), &s)
	var out []string
	for _, d := range s.Denied {
		out = append(out, d.Name)
	}
	return out
}

// callOf is the latest call with session among its arguments: a
// subagent's, which its record names.
func callOf(calls func() [][]string, session string) []string {
	all := calls()
	for i := len(all) - 1; i >= 0; i-- {
		if slices.Contains(all[i], session) {
			return all[i]
		}
	}
	return nil
}

// reported tells whether the subagent name has reported.
func reported(a *Agent, name string) func() bool {
	return func() bool { r, _ := a.loadRecord(name); return r.State == "resting" || r.State == "ended" }
}

// mcpAgentsChat is an agent with the user's setup, delta turned off and
// beta and gamma for subagents only, after one turn: Opt's calls have seen
// the servers.
func mcpAgentsChat(t *testing.T) (*Agent, func() [][]string, *recorder) {
	t.Helper()
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	a, calls := fakeAgent(t, c)
	a.report = func(string) {}
	a.ownConfig, a.mcpOff, a.mcpAgents = true, []string{"delta"}, []string{"beta", "gamma"}
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	return a, calls, r
}

// Each call's own settings keep out the servers turned off and those for
// subagents only it was not given: Opt's turn has neither, a subagent
// spawned with beta has beta. Nothing is switched in Claude Code, which
// would keep the switch in the user's own config for the folder.
func TestMCPForSubagents(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	if got := denied(calls()[0]); !slices.Equal(got, []string{"delta", "beta", "gamma"}) {
		t.Fatalf("Opt's turn denies %q", got)
	}
	a.Btw(message{kind: "user", text: "side-ask one"})
	if all := calls(); !slices.Equal(denied(all[len(all)-1]), []string{"delta", "beta", "gamma"}) {
		t.Fatalf("a side question denies %q", denied(all[len(all)-1]))
	}
	a.Spawn("Plain", "stream-reply", "", "", 0, nil)
	if out := a.Spawn("Granted", "stream-reply", "", "", 0, nil, "beta"); !strings.Contains(out, "Started [Granted]") || !strings.Contains(out, "beta") {
		t.Fatalf("spawn: %s", out)
	}
	// alpha is on for all: granting it changes nothing, and is no error.
	if out := a.Spawn("Alpha", "stream-reply", "", "", 0, nil, "alpha"); !strings.Contains(out, "Started [Alpha]") {
		t.Fatalf("spawn with alpha: %s", out)
	}
	for _, tc := range []struct {
		name      string
		deny, mcp []string
	}{
		{"Plain", []string{"delta", "beta", "gamma"}, nil},
		{"Granted", []string{"delta", "gamma"}, []string{"beta"}},
		{"Alpha", []string{"delta", "beta", "gamma"}, nil},
	} {
		waitFor(t, tc.name+"'s report", reported(a, tc.name))
		rec, _ := a.loadRecord(tc.name)
		if call := callOf(calls, rec.Session); !strings.Contains(flagOf(call, "--tools"), "Artifact") {
			t.Fatalf("%s's tools: %s", tc.name, flagOf(call, "--tools"))
		}
		if got := denied(callOf(calls, rec.Session)); !slices.Equal(got, tc.deny) || !slices.Equal(rec.MCP, tc.mcp) {
			t.Fatalf("%s denies %q, record %q", tc.name, got, rec.MCP)
		}
	}
	if ctl := strings.Join(fakeFiles(a, ".ctl"), "\n"); strings.Contains(ctl, `"mcp_toggle"`) {
		t.Fatalf("a server switched in Claude Code:\n%s", ctl)
	}
}

// Servers that can't be granted: one turned off, one the user doesn't have,
// any without the user's setup. The answer names those that can be.
func TestMCPGrantErrors(t *testing.T) {
	t.Parallel()
	a, _, _ := mcpAgentsChat(t)
	for _, tc := range []struct{ server, want string }{
		{"delta", "delta is off"},
		{"ghost", "no MCP server ghost"},
	} {
		out := a.Spawn("Bad", "stream-reply", "", "", 0, nil, tc.server)
		if !strings.Contains(out, tc.want) || !strings.Contains(out, "beta, gamma") || strings.Contains(out, "Started") {
			t.Fatalf("%s: %s", tc.server, out)
		}
		if out := a.Tell("Bad", "go on", tc.server); !strings.Contains(out, tc.want) {
			t.Fatalf("tell %s: %s", tc.server, out)
		}
	}
	if out := a.Tell("Nobody", "hi", "beta"); !strings.Contains(out, "No subagent [Nobody]") || strings.Contains(out, "Granted") {
		t.Fatalf("tell no one: %s", out)
	}
	a.ownConfig = false
	if out := a.Spawn("Bare", "stream-reply", "", "", 0, nil, "beta"); !strings.Contains(out, "-claude-config") || strings.Contains(out, "Started") {
		t.Fatalf("without the user's setup: %s", out)
	}
}

// A server granted that has failed: the subagent starts, and the user is
// warned, with what to do.
func TestMCPGrantFailed(t *testing.T) {
	t.Parallel()
	a, _, r := mcpAgentsChat(t)
	a.Spawn("Broken", "stream-reply", "", "", 0, nil, "gamma")
	waitFor(t, "Broken's report", reported(a, "Broken"))
	if !slices.ContainsFunc(r.warnings(), func(s string) bool {
		return strings.Contains(s, "[Broken]") && strings.Contains(s, "gamma") && strings.Contains(s, "failed") && strings.Contains(s, "/mcp")
	}) {
		t.Fatalf("warnings: %q", r.warnings())
	}
}

// A server can't come into a call underway, so tell's grant restarts the
// subagent's call from its saved session, with the server and the message:
// one at work at its next step, one at rest at once. Another tell meanwhile
// comes with it.
func TestMCPGrantRestarts(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	got := make(chan string, 4)
	a.report = func(text string) { got <- text }
	for _, tc := range []struct{ name, task string }{{"Worker", "step-task"}, {"Rester", "stream-reply"}} {
		a.Spawn(tc.name, tc.task, "", "", 0, nil)
		if tc.name == "Worker" {
			waitFor(t, "Worker at its steps", func() bool { steps, _, _ := a.steps("Worker"); return len(steps) > 3 })
		} else {
			<-got
		}
		first, _ := a.loadRecord(tc.name)
		if out := a.Tell(tc.name, "use beta now", "beta"); !strings.Contains(out, "Granted it: beta") || !strings.Contains(out, "restart") {
			t.Fatalf("%s tell: %s", tc.name, out)
		}
		second := tc.name == "Worker" // at work, its restart waits for its next step; at rest, it may be starting again already
		if out := a.Tell(tc.name, "and this too"); second && !strings.Contains(out, "restart") {
			t.Fatalf("%s second tell: %s", tc.name, out)
		}
		var report string
		select {
		case report = <-got:
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: no report after the restart", tc.name)
		}
		if !strings.Contains(report, "restarted your call") || !strings.Contains(report, "use beta now") || second && !strings.Contains(report, "and this too") {
			t.Fatalf("%s report: %s", tc.name, report)
		}
		all := calls()
		again := all[len(all)-1]
		if flagOf(again, "--resume") != first.Session || slices.Contains(denied(again), "beta") {
			t.Fatalf("%s restarted with %q", tc.name, again)
		}
		if rec, _ := a.loadRecord(tc.name); !slices.Equal(rec.MCP, []string{"beta"}) {
			t.Fatalf("%s record: %+v", tc.name, rec)
		}
		steps, _, _ := a.steps(tc.name)
		if n := len(slices.DeleteFunc(steps, func(s step) bool { return s.Kind != "opt" || s.Text != "use beta now" })); n != 1 {
			t.Fatalf("%s's log has Opt's message %d times", tc.name, n)
		}
	}
}

// A tell that grants a server the call has already restarts nothing, at
// work or at rest.
func TestMCPGrantHadNoRestart(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	got := make(chan string, 4)
	a.report = func(text string) { got <- text }
	for _, tc := range []struct{ name, task string }{{"Worker", "step-task"}, {"Rester", "stream-reply"}} {
		a.Spawn(tc.name, tc.task, "", "", 0, nil, "beta")
		if tc.name == "Worker" {
			waitFor(t, "Worker at its steps", func() bool { steps, _, _ := a.steps("Worker"); return len(steps) > 3 })
		} else {
			<-got
		}
		n := len(calls())
		if out := a.Tell(tc.name, "use beta", "beta"); strings.Contains(out, "restart") {
			t.Fatalf("%s tell: %s", tc.name, out)
		}
		if tc.name == "Rester" {
			<-got
		}
		a.mu.Lock()
		h := a.halts[tc.name]
		restart := h != nil && h.restart != nil
		a.mu.Unlock()
		if restart || len(calls()) != n {
			t.Fatalf("%s restarted: %v, calls %d, then %d", tc.name, restart, n, len(calls()))
		}
	}
}

// A tell refused, as to a subagent starting, grants nothing: Opt tells it
// again, servers and all.
func TestMCPGrantStarting(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	a.Spawn("Early", "stream-reply", "", "", 0, nil)
	out := a.Tell("Early", "use beta", "beta")
	waitFor(t, "Early to report", reported(a, "Early"))
	rec, _ := a.loadRecord("Early")
	call := callOf(calls, rec.Session)
	if strings.Contains(out, "is starting") != slices.Contains(denied(call), "beta") || strings.Contains(out, "is starting") != (rec.MCP == nil) {
		t.Fatalf("tell %q; its call denies %q, record %q", out, denied(call), rec.MCP)
	}
}

// A server granted as the subagent compacts mid-task reaches it once
// compacted: its call restarts.
func TestMCPGrantCompacting(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	a.Spawn("Busy", "step-task", "", "", 0, nil)
	waitFor(t, "its steps", func() bool { steps, _, _ := a.steps("Busy"); return len(steps) > 3 })
	if _, ok := a.CompactAgent("Busy"); !ok {
		t.Fatal("no compaction")
	}
	waitFor(t, "the interrupt", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.resting["Busy"] != nil && a.resting["Busy"].goOn
	})
	if out := a.Tell("Busy", "use beta", "beta"); !strings.Contains(out, "is compacting") || !strings.Contains(out, "Granted it: beta") {
		t.Fatalf("tell: %s", out)
	}
	rec, _ := a.loadRecord("Busy")
	waitFor(t, "a call with beta", func() bool {
		call := callOf(calls, rec.Session)
		return flagOf(call, "--resume") == rec.Session && !slices.Contains(denied(call), "beta")
	})
}

// A subagent resumed from its saved session keeps the servers it was
// given, and a tell that resumes one can give it more.
func TestMCPGrantResume(t *testing.T) {
	t.Parallel()
	a, calls, _ := mcpAgentsChat(t)
	a.rest = 300 * time.Millisecond
	ended := func(name string) func() bool {
		return func() bool {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.subs[name] != nil && a.subs[name].State == "ended"
		}
	}
	a.Spawn("Keeper", "stream-reply", "", "", 0, nil, "beta")
	a.Spawn("Later", "stream-reply", "", "", 0, nil)
	waitFor(t, "both to end", func() bool { return ended("Keeper")() && ended("Later")() })
	for _, tc := range []struct {
		name string
		mcp  []string
	}{{"Keeper", nil}, {"Later", []string{"beta"}}} {
		if out := a.Tell(tc.name, "go on", tc.mcp...); !strings.HasPrefix(out, "Resuming ["+tc.name+"]") {
			t.Fatalf("tell %s: %s", tc.name, out)
		}
		rec, _ := a.loadRecord(tc.name)
		waitFor(t, tc.name+"'s resumed call to end", func() bool { // ended at once: not yet resumed
			return flagOf(callOf(calls, rec.Session), "--resume") == rec.Session && ended(tc.name)()
		})
		rec, _ = a.loadRecord(tc.name)
		if got := denied(callOf(calls, rec.Session)); !slices.Equal(got, []string{"delta", "gamma"}) || !slices.Equal(rec.MCP, []string{"beta"}) {
			t.Fatalf("%s resumed denies %q, record %q", tc.name, got, rec.MCP)
		}
	}
}

// Reconnect reaches the page's call and each call underway that has the
// server; the status says who has what; turning a server on and back
// keeps it with those given it.
func TestMCPReconnectAndStatus(t *testing.T) {
	t.Parallel()
	a, _, _ := mcpAgentsChat(t)
	a.Spawn("Worker", "slow-task", "", "", 0, nil, "beta")
	waitFor(t, "Worker at work", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.running["Worker"] != nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := a.mcpReconnect(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reconnects", func() bool {
		return strings.Count(strings.Join(fakeFiles(a, ".ctl"), "\n"), `"mcp_reconnect"`) >= 2 // the page's call and Worker's
	})
	if out := statusCommand(a); !strings.Contains(out, "MCP servers for subagents only: beta (granted to [Worker]), gamma") {
		t.Fatalf("status:\n%s", out)
	}
	for _, state := range []string{"on", "agents"} {
		if err := a.setMCP(ctx, "beta", state); err != nil {
			t.Fatal(err)
		}
	}
	a.mu.Lock()
	keeps := !slices.Contains(a.mcpOffLocked("Worker"), "beta") && slices.Contains(a.mcpOffLocked(""), "beta")
	a.mu.Unlock()
	if !keeps {
		t.Fatal("Worker lost beta, or Opt has it")
	}
	if ctl := strings.Join(fakeFiles(a, ".ctl"), "\n"); strings.Contains(ctl, `"mcp_toggle"`) {
		t.Fatalf("a server switched in Claude Code:\n%s", ctl)
	}
}

// spawn and tell offer the servers for subagents only, by name; with none,
// their schemas are as before, so Opt's cached prompt stays the same.
func TestMCPSpawnSchema(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	desc := func() map[string]string {
		out := map[string]string{}
		for _, tl := range a.mcpList() {
			props := tl["inputSchema"].(map[string]any)["properties"].(map[string]any)
			if m, ok := props["mcp"].(map[string]any); ok {
				out[tl["name"].(string)], _ = m["description"].(string)
			}
		}
		return out
	}
	if d := desc(); len(d) != 0 {
		t.Fatalf("with none: %v", d)
	}
	a.mcpAgents = []string{"beta", "gamma"}
	d := desc()
	if !strings.Contains(d["spawn"], "beta, gamma") || !strings.Contains(d["tell"], "beta, gamma") || len(d) != 2 {
		t.Fatalf("with beta and gamma: %v", d)
	}
}
