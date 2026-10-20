package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
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
	if flagOf(args, "--setting-sources") != "user,project,local" || flagOf(args, "--settings") != `{"disableAllHooks":true,"autoCompactEnabled":false}` ||
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
	a.Compact(context.Background(), []string{"x"}, 0, []block{textBlock("Compaction: x")})
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
	// Opt's own: kept in settings.json, and in the next call's settings;
	// the user's Claude Code keeps its own.
	if s := loadSettings(a.chat.dir); s.Skills["one"] != "off" {
		t.Fatalf("settings: %+v", s)
	}
	a.mu.Lock()
	set := a.ownSettings()
	a.mu.Unlock()
	if set != `{"disableAllHooks":true,"autoCompactEnabled":false,"skillOverrides":{"one":"off"}}` {
		t.Fatalf("a call's settings: %s", set)
	}
	ws.send(map[string]any{"t": "skills", "action": "set", "name": "plug:two", "state": "off"})
	if u := ws.next("locked", typ("skills")); !strings.Contains(u["error"].(string), "set by plugin") {
		t.Fatalf("a locked skill: %v", u)
	}
	// Plugins: listed once each, with what they bring; one turned off for
	// Opt goes to the next call's settings, and the control call starts
	// again with it.
	ws.send(map[string]any{"t": "plugins"})
	u = ws.next("plugins", typ("plugins"))
	if ps := u["plugins"].([]any); len(ps) != 2 || ps[0].(map[string]any)["skills"] != 1.0 || ps[0].(map[string]any)["tokens"] != 20.0 ||
		ps[0].(map[string]any)["on"] != true || ps[1].(map[string]any)["on"] != false {
		t.Fatalf("plugins: %v", u)
	}
	ctl := a.ctl
	ws.send(map[string]any{"t": "plugins", "action": "off", "name": "plug@market"})
	if u := ws.next("plugin off", typ("plugins")); u["plugins"].([]any)[0].(map[string]any)["on"] != false || u["error"] != nil {
		t.Fatalf("after off: %v", u)
	}
	a.mu.Lock()
	set = a.ownSettings()
	a.mu.Unlock()
	if s := loadSettings(a.chat.dir); s.Plugins["plug@market"] != false || len(s.Plugins) != 1 ||
		set != `{"disableAllHooks":true,"autoCompactEnabled":false,"skillOverrides":{"one":"off"},"enabledPlugins":{"plug@market":false}}` {
		t.Fatalf("settings: %+v; a call's: %s", s, set)
	}
	a.ctlMu.Lock()
	again := a.ctl
	a.ctlMu.Unlock()
	if again == ctl {
		t.Fatal("the control call goes on with the old plugins")
	}
	ws.send(map[string]any{"t": "plugins", "action": "off", "name": "nope@market"})
	if u := ws.next("no such plugin", typ("plugins")); !strings.Contains(u["error"].(string), "no plugin") {
		t.Fatalf("a missing plugin: %v", u)
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

// With -claude-md, a turn loads the CLAUDE.md files of the folders it works
// in, never the user's own ~/.claude/CLAUDE.md; memory stays off.
func TestClaudeMd(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	a, calls := fakeAgent(t, c)
	a.ownConfig, a.claudeMd = true, true
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]string{filepath.Join(claudeConfigDir(), "CLAUDE.md")})
	want := `{"disableAllHooks":true,"autoCompactEnabled":false,"claudeMdExcludes":` + string(b) + `}`
	if got := flagOf(calls()[0], "--settings"); got != want {
		t.Fatalf("settings: %s, want %s", got, want)
	}
	env := fakeFiles(a, ".env")[0]
	if strings.Contains(env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS") || !strings.Contains(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1") {
		t.Fatalf("turn env: %s", env)
	}
}
