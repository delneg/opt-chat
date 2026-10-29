package main

import (
	"context"
	"strings"
	"testing"
)

// Every result's spend since the last one is kept in usage/, a resumed
// session's counted from where its last run left off; /usage shows the
// plan from get_usage, then the calls' spend by model and by caller.
func TestUsage(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	all, err := readSpend(c.dir, started)
	if err != nil {
		t.Fatal(err)
	}
	var cost float64
	for _, s := range all {
		if s.Role != "turn" || s.Requests != 1 || s.Models["claude-big"].Output != 10 {
			t.Fatalf("%+v", s)
		}
		cost += s.Cost
	}
	if len(all) == 0 || cost != 0.25*float64(len(all)) {
		t.Fatalf("%d results, $%v", len(all), cost)
	}
	p, err := a.start(a.life, "big", "", roleAgent, "--resume", "s1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.close(); p.wait() })
	if p.spent.Cost != 0.5 || p.spent.Models["claude-big"].Output != 500 {
		t.Fatalf("resumed from %+v", p.spent)
	}

	out, ok, err := command(a, "/usage")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	for _, want := range []string{
		"Your plan (max):\n",
		"  Current session             ██░░░░░░░░░░░░░░░░░░   9% used, resets ",
		"  Current week (Fable)        █████░░░░░░░░░░░░░░░  27% used",
		"  Usage credits               $1.25 of $50.00 spent\n",
		"  The week by where it was used: Claude Code 96%, Chats 1%\n",
		"All of Claude Code on this machine, last 24h (2,947 requests, 23 sessions):\n",
		"  Usage came from: long context 67%\n",
		"  MCP servers:    plugin:playwright:playwright 11%\n",
		"opt-chat's calls today: $",
		"% of input read from the cache\n",
		"  claude-big:          ",
		"  The chat's turns:    $",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	if _, _, err := command(a, "/usage month"); err == nil {
		t.Fatal("/usage month taken")
	}
	if out, _, _ := command(a, "/usage week"); strings.Contains(out, "All of Claude Code") || !strings.Contains(out, "opt-chat's calls in the last 7 days: $") {
		t.Fatal(out)
	}
}
