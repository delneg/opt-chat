package main

import (
	"regexp"
	"strings"
	"testing"
)

// Opt lets a subagent start and tell subagents of its own (spawn's
// spawn): their reports go to it, not to Opt, and Opt's chat notes each
// one. Its own can't start any; one Opt didn't let can't; it tells only
// its own; and the reports of one whose parent the user stopped go to Opt.
func TestSubagentSpawns(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	a.agentMCP = `{"url":"x?agent"}`
	report, quiet := loopReports(t, a)
	as := func(caller, tool string, x toolArgs) string {
		t.Helper()
		s, _ := callTool(a, caller != "", caller, tool, x)
		return s
	}

	if got := as("", "spawn", toolArgs{Name: "Lead", Task: "wait-for-message", Spawns: true}); !strings.HasPrefix(got, "Started [Lead]") {
		t.Fatal(got)
	}
	as("", "spawn", toolArgs{Name: "Plain", Task: "wait-for-message"})
	waitFor(t, "their calls", func() bool { return len(calls()) == 2 })
	var args []string
	for _, call := range calls() {
		args = append(args, strings.Join(call, " "))
	}
	for _, name := range []string{"Lead", "Plain"} { // each its own address: the server knows who calls
		if !strings.Contains(strings.Join(args, "\n"), `"url":"x?agent=`+name+`"`) {
			t.Fatalf("%s's call: %s", name, args)
		}
	}
	for caller, want := range map[string]string{
		"Plain": "Only Opt starts and tells subagents",
		"?":     "Only Opt starts and tells subagents",
	} {
		if got := as(caller, "spawn", toolArgs{Name: "X", Task: "t"}); !strings.HasPrefix(got, want) {
			t.Fatalf("%s's spawn: %q", caller, got)
		}
	}
	if got := as("Lead", "spawn", toolArgs{Name: "Deep", Task: "t", Spawns: true}); got != "Your subagents can't start their own." {
		t.Fatal(got)
	}
	if got := as("Lead", "tell", toolArgs{Name: "Plain", Message: "m"}); !strings.HasPrefix(got, "You tell only the subagents you started") {
		t.Fatal(got)
	}

	if got := as("Lead", "spawn", toolArgs{Name: "Help", Task: "look"}); !strings.HasPrefix(got, "Started [Help]") {
		t.Fatal(got)
	}
	waitFor(t, "its record", func() bool { rec, _ := a.loadRecord("Help"); return rec.Session != "" })
	if rec, _ := a.loadRecord("Help"); rec.Parent != "Lead" || rec.Spawns {
		t.Fatalf("its record: %+v", rec)
	}
	if rec, _ := a.loadRecord("Lead"); !rec.Spawns {
		t.Fatalf("its parent's record: %+v", rec)
	}
	got := report() // Help's report went to Lead, whose answer comes to Opt
	if !strings.HasPrefix(got, "[Lead] got: From [Help], your subagent:\n[Help] answer to") {
		t.Fatalf("report: %q", got)
	}
	quiet()
	log := strings.Join(logged(c), "\n")
	if !strings.Contains(log, "[opt-chat] [Lead] started subagent [Help] of its own") || strings.Contains(log, "\nwork: [Help]") {
		t.Fatalf("Opt's chat: %q", log)
	}
	if l := c.ZoomAgent("Lead"); !regexp.MustCompile(`sub(@\d+)?: From \[Help\], your subagent:`).MatchString(l) {
		t.Fatalf("Lead's log: %q", l)
	}

	// A tell to its own goes, marked as its parent's.
	if got := as("Lead", "tell", toolArgs{Name: "Help", Message: "more"}); !strings.HasPrefix(got, "Resuming [Help]") && !strings.HasPrefix(got, "Sent") {
		t.Fatal(got)
	}
	waitFor(t, "the tell in Help's log", func() bool {
		return regexp.MustCompile(`sub(@\d+)?: From \[Lead\], the subagent that started you:\nmore`).MatchString(c.ZoomAgent("Help"))
	})

	// Its parent stopped by the user: a report comes to Opt.
	as("", "spawn", toolArgs{Name: "Boss", Task: "wait-for-stop", Spawns: true})
	waitFor(t, "Boss at work", func() bool { return strings.Contains(c.ZoomAgent("Boss"), "working") })
	as("Boss", "spawn", toolArgs{Name: "Kid", Task: "wait-for-message"})
	waitFor(t, "Kid at work", func() bool { return strings.Contains(c.ZoomAgent("Kid"), "waiting") })
	a.StopAgent("Boss")
	waitFor(t, "Boss stopped", func() bool { rec, _ := a.loadRecord("Boss"); return rec.State == "halted" })
	a.Tell("Kid", "go")
	for {
		got := report()
		if strings.HasPrefix(got, "[Kid] got: go") {
			break
		}
		if !strings.HasPrefix(got, "[Lead]") { // Help's second report, through Lead
			t.Fatalf("report: %q", got)
		}
	}
}
