package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeFiles reads what the fake Claude Code recorded beside its calls'
// arguments (ext ".env", ".ctl" or ".skills"), a line each.
func fakeFiles(a *Agent, ext string) []string {
	b, _ := os.ReadFile(fakeArgsFile[a] + ext)
	return slices.DeleteFunc(strings.Split(string(b), "\n"), func(s string) bool { return s == "" })
}

// With the user's own setup, a turn loads their settings, skills and MCP
// servers but never their hooks, CLAUDE.md or memory; it waits for its
// servers and turns off those turned off here before it sends the view.
// Compactions stay bare.
func TestOwnConfig(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	for range 70 {
		c.Log("note", "note")
	}
	record(c)
	a, calls := fakeAgent(t, c)
	a.ownConfig, a.mcpOff = true, []string{"beta", "ghost"}
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	args := calls()[0]
	if flagOf(args, "--setting-sources") != "user,project,local" || flagOf(args, "--settings") != noHooks ||
		flagOf(args, "--tools") != "Read,Skill,ToolSearch" || slices.Contains(args, "--strict-mcp-config") || slices.Contains(args, "--disable-slash-commands") {
		t.Fatalf("turn args: %q", args)
	}
	env := fakeFiles(a, ".env")[0]
	for _, kv := range []string{"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "ENABLE_TOOL_SEARCH=true", "CLAUDE_CODE_TETHER_LIVE=0"} {
		if !strings.Contains(env, kv) {
			t.Fatalf("turn env lacks %s: %s", kv, env)
		}
	}
	// beta turned off, ghost (gone from the user's setup) never waited
	// for, and the status asked until alpha connected.
	ctl := strings.Join(fakeFiles(a, ".ctl"), "\n")
	if !strings.Contains(ctl, `"beta"`) || strings.Contains(ctl, `"ghost"`) || strings.Count(ctl, `"mcp_status"`) != 3 {
		t.Fatalf("control requests before the view:\n%s", ctl)
	}

	a.ownConfig = false // a compaction: bare, as ever
	a.Compact(context.Background(), []string{"x"}, []block{textBlock("Compaction: x")})
	if args := calls()[1]; flagOf(args, "--setting-sources") != "" || !slices.Contains(args, "--strict-mcp-config") {
		t.Fatalf("compaction args: %q", args)
	}
}

// The panels: MCP servers listed without their config (it can hold keys)
// or the chat's own; one turned off for good; a sign-in page; skills
// listed and one turned off through Claude Code's own command.
func TestWebConfig(t *testing.T) {
	wt := startWeb(t)
	a := wt.h.agent
	a.ownConfig = true
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "mcp"})
	u := ws.next("mcp", typ("mcp"))
	b, _ := json.Marshal(u)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), `"opt"`) || len(u["servers"].([]any)) != 2 {
		t.Fatalf("mcp: %s", b)
	}
	ws.send(map[string]any{"t": "mcp", "action": "off", "name": "beta"})
	u = ws.next("mcp off", typ("mcp"))
	if s := u["servers"].([]any)[1].(map[string]any); s["off"] != true || s["status"] != "disabled" || u["error"] != nil {
		t.Fatalf("after off: %v", u)
	}
	if s := loadSettings(wt.c.dir); !slices.Equal(s.MCPOff, []string{"beta"}) {
		t.Fatalf("settings: %+v", s)
	}
	ws.send(map[string]any{"t": "mcp", "action": "signin", "name": "alpha"})
	if u := ws.next("sign-in", typ("mcp")); u["signIn"] != "https://auth.example/alpha" || u["name"] != "alpha" {
		t.Fatalf("sign-in: %v", u)
	}

	ws.send(map[string]any{"t": "skills"})
	if u := ws.next("skills", typ("skills")); len(u["skills"].([]any)) != 2 {
		t.Fatalf("skills: %v", u)
	}
	ws.send(map[string]any{"t": "skills", "action": "set", "name": "one", "state": "off"})
	u = ws.next("skill off", typ("skills"))
	if s := u["skills"].([]any)[0].(map[string]any); s["state"] != "off" || u["error"] != nil {
		t.Fatalf("after off: %v", u)
	}
	var edit struct {
		Name    string            `json:"name"`
		State   string            `json:"state"`
		Handles map[string]string `json:"handles"`
	}
	if edits := fakeFiles(a, ".skills"); len(edits) != 1 || json.Unmarshal([]byte(edits[0]), &edit) != nil ||
		edit.Name != "one" || edit.State != "off" || edit.Handles["unqualified_name"] != "one" {
		t.Fatalf("edits: %q", edits)
	}
	ws.send(map[string]any{"t": "skills", "action": "set", "name": "nope", "state": "off"})
	if u := ws.next("no such skill", typ("skills")); !strings.Contains(u["error"].(string), "no skill") {
		t.Fatalf("a missing skill: %v", u)
	}

	a.ownConfig = false
	ws.send(map[string]any{"t": "mcp"})
	if u := ws.next("bare", typ("mcp")); !strings.Contains(u["error"].(string), "-claude-config=false") {
		t.Fatalf("bare: %v", u)
	}
}
