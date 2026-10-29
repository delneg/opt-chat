package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// /status: Claude Code's version and account, then the run's settings,
// the view, the last turn and the plan's usage, Claude Code asked for the
// usage; with Claude Code down the rest still comes.
func TestStatusCommand(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.hub = newHub()
	a, _ := fakeAgent(t, c)
	a.recall, a.ownConfig, a.mcpUp, a.mcpSeen = 8, true, []string{"opt", "alpha"}, []string{"opt", "alpha", "beta"}
	if !asks(a, "/status") {
		t.Fatal("/status answered at once")
	}
	c.hub.ended(turnEnd{Context: 45_100, Turn: usage{Read: 900, Write: 50, Input: 50, Output: 10}})
	out, ok, err := command(a, "/STATUS")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	for _, want := range []string{
		"Claude Code 9.9.9, signed in as me@example.com (Claude Max, firstParty)\n",
		"Chat: " + c.dir + ", 0 messages; Opt's tools run in " + a.cwd + "\n",
		"View: 0 lines, 0.0 KB (merged down to 64 KB past 128 KB)\n",
		"Turns: big, effort , mode auto; recall 8 messages\n",
		"Subagents: mid, effort ; autocompact off\n",
		"Compactions: small, effort \n",
		": context 45.1k; cache read 0.9k, write 0.1k, in 0.1k, out 0.0k; 90% of input read from the cache\n",
		"Usage: five-hour 12%, resets ",
		"MCP servers at the last call: opt, alpha connected; not: beta",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q in:\n%s", want, out)
		}
	}
	a.agentWindow, a.resting = 250_000, map[string]*rest{"Ann": nil}
	a.running["Scout"] = nil
	a.mu.Lock()
	a.claude = filepath.Join(t.TempDir(), "claude") // none there
	a.mu.Unlock()
	a.quit()
	out, _, _ = command(a, "/status")
	if !strings.HasPrefix(out, "Claude Code: ") || !strings.Contains(out, "autocompact past 250k tokens; at work: [Scout]; reported: [Ann]\n") {
		t.Fatal(out)
	}
}
