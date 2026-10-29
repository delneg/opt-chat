package main

import (
	"context"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeJSON writes v to path as JSON.
func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// mcpNames is the servers an MCP config file names, with their configs.
func mcpNames(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	var f struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f.Servers
}

// Opt's own servers start as a copy of those Claude Code gives the folder
// Opt worked in: the user's and the folder's own, the folder's wins on a
// name. The servers Claude Code keeps off there stay off, in Opt's own
// list. Made once: a file there is Opt's, never written again.
func TestSeedOptMCP(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cj := filepath.Join(tmp, ".claude.json")
	from := "/home/someone"
	writeJSON(t, cj, map[string]any{
		"mcpServers": map[string]any{"user1": map[string]any{"command": "u", "env": map[string]any{"KEY": "k1"}}, "shared": map[string]any{"command": "user"}},
		"projects": map[string]any{
			from:         map[string]any{"mcpServers": map[string]any{"shared": map[string]any{"command": "local"}, "local1": map[string]any{"type": "http", "url": "https://x"}}, "disabledMcpServers": []string{"local1", "claude.ai Mail", "plugin:f:f", "kept"}},
			"/elsewhere": map[string]any{"mcpServers": map[string]any{"far": map[string]any{"command": "f"}}},
		},
	})
	dir := filepath.Join(tmp, "chat")
	os.MkdirAll(dir, 0o700)
	if err := saveSettings(dir, settings{MCPOff: []string{"kept"}, MCPAgents: []string{"plugin:f:f"}}); err != nil {
		t.Fatal(err)
	}
	if err := seedOptMCP(dir, cj, from); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "mcp.json")
	got := mcpNames(t, dst)
	if len(got) != 3 || got["shared"]["command"] != "local" || got["user1"]["env"].(map[string]any)["KEY"] != "k1" || got["local1"]["url"] != "https://x" {
		t.Fatalf("copied: %v", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v: it holds keys", fi.Mode())
	}
	if s := loadSettings(dir); !slices.Equal(s.MCPOff, []string{"kept", "local1", "claude.ai Mail"}) || !slices.Equal(s.MCPAgents, []string{"plugin:f:f"}) {
		t.Fatalf("settings: %+v", s)
	}

	// Once there, the file is Opt's: never copied again, nothing turned off.
	writeJSON(t, dst, map[string]any{"mcpServers": map[string]any{"mine": map[string]any{"command": "m"}}})
	saveSettings(dir, settings{})
	if err := seedOptMCP(dir, cj, from); err != nil {
		t.Fatal(err)
	}
	if got := mcpNames(t, dst); len(got) != 1 || got["mine"] == nil {
		t.Fatalf("written again: %v", got)
	}
	if s := loadSettings(dir); s.MCPOff != nil {
		t.Fatalf("turned off again: %+v", s)
	}

	// No Claude Code config: an empty list, to add to.
	other := filepath.Join(tmp, "other")
	if err := seedOptMCP(other, filepath.Join(tmp, "none.json"), from); err != nil {
		t.Fatal(err)
	}
	if got := mcpNames(t, filepath.Join(other, "mcp.json")); len(got) != 0 {
		t.Fatalf("from nothing: %v", got)
	}
	// One it can't read is an error, and leaves no file.
	bad := filepath.Join(tmp, "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	if err := seedOptMCP(filepath.Join(tmp, "third"), bad, from); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(tmp, "third", "mcp.json")); err == nil {
		t.Fatal("written from a bad config")
	}
}

// Every call with the user's setup loads Opt's own file and keeps out
// Claude Code's own servers, the user's, the folder's and its projects',
// unless Opt's file has one by the same name: then Opt's is the one
// loaded. Opt's own "opt" is never kept out. The folder is made if
// missing: the system cleans its temp folder.
func TestOptMCPCalls(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	tmp := t.TempDir()
	a.ownConfig, a.mcpOff, a.mcpAgents = true, []string{"delta"}, []string{"beta"}
	a.cwd = filepath.Join(tmp, "a", "b")
	a.optMCP = filepath.Join(tmp, "mcp.json")
	a.claudeJSON = filepath.Join(tmp, ".claude.json")
	writeJSON(t, a.optMCP, map[string]any{"mcpServers": map[string]any{"shared": map[string]any{"command": "opt-copy"}, "beta": map[string]any{"command": "b"}}})
	real, _ := filepath.EvalSymlinks(tmp)
	writeJSON(t, a.claudeJSON, map[string]any{
		"mcpServers": map[string]any{"user1": map[string]any{}, "shared": map[string]any{}, "opt": map[string]any{}},
		"projects": map[string]any{
			filepath.Join(real, "a", "b"): map[string]any{"mcpServers": map[string]any{"local1": map[string]any{}}},
			"/elsewhere":                  map[string]any{"mcpServers": map[string]any{"far": map[string]any{}}},
		},
	})
	os.MkdirAll(filepath.Join(tmp, "a"), 0o700)
	writeJSON(t, filepath.Join(tmp, "a", ".mcp.json"), map[string]any{"mcpServers": map[string]any{"proj1": map[string]any{}}})

	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(a.cwd); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("folder: %v %v", fi, err)
	}
	cc := []string{"local1", "proj1", "user1"}
	turn := calls()[0]
	if got := mcpConfigs(turn); !slices.Equal(got, []string{"turn-mcp", a.optMCP}) {
		t.Fatalf("turn's configs: %q", got)
	}
	if got := denied(turn); !slices.Equal(got, append([]string{"delta", "beta"}, cc...)) {
		t.Fatalf("turn denies %q", got)
	}

	a.Spawn("Granted", "stream-reply", "", "", 0, nil, "beta")
	waitFor(t, "its report", reported(a, "Granted"))
	rec, _ := a.loadRecord("Granted")
	sub := callOf(calls, rec.Session)
	if got := mcpConfigs(sub); !slices.Equal(got, []string{"agent-mcp", a.optMCP}) {
		t.Fatalf("subagent's configs: %q", got)
	}
	if got := denied(sub); !slices.Equal(got, append([]string{"delta"}, cc...)) {
		t.Fatalf("subagent denies %q", got)
	}

	// The page's call lists Opt's servers as they are: off ones too, none
	// of Claude Code's own.
	if _, err := a.mcpServers(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := calls()
	ctl := all[len(all)-1]
	if got := mcpConfigs(ctl); !slices.Equal(got, []string{"turn-mcp", a.optMCP}) {
		t.Fatalf("control's configs: %q", got)
	}
	if got := denied(ctl); !slices.Equal(got, cc) {
		t.Fatalf("control denies %q", got)
	}

	// No file: none of Opt's own, and still none of Claude Code's.
	os.Remove(a.optMCP)
	a.Btw(message{kind: "user", text: "side-ask"})
	all = calls()
	btw := all[len(all)-1]
	if got := mcpConfigs(btw); !slices.Equal(got, []string{"turn-mcp"}) {
		t.Fatalf("btw's configs: %q", got)
	}
	if got := denied(btw); !slices.Equal(got, []string{"delta", "beta", "local1", "proj1", "shared", "user1"}) {
		t.Fatalf("btw denies %q", got)
	}
}

// mcpConfigs is the files a call's --mcp-config names.
func mcpConfigs(args []string) []string {
	i := slices.Index(args, "--mcp-config")
	if i < 0 {
		return nil
	}
	var out []string
	for _, s := range args[i+1:] {
		if len(s) > 1 && s[:2] == "--" {
			break
		}
		out = append(out, s)
	}
	return out
}

// @ completes paths from the user's home, wherever Opt's tools run.
func TestWebFilesRoot(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	a := wt.h.agent
	a.files = t.TempDir()
	os.WriteFile(filepath.Join(a.files, "zebra.txt"), nil, 0o600)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "files", "seq": 1, "text": "zeb"})
	u := ws.next("files", typ("files"))
	items, _ := u["items"].([]any)
	if len(items) != 1 || filepath.Base(items[0].(map[string]any)["path"].(string)) != "zebra.txt" {
		t.Fatalf("files: %v", u)
	}
}
