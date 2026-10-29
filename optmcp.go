package main

import (
	"cmp"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Opt's own MCP servers are a file of its own, mcp.json in the chat's
// folder, in Claude Code's format: every call with the user's setup loads
// it (--mcp-config), as Claude Code reads it then, and keeps out Claude
// Code's own servers, the user's and those of the folder Opt works in
// (deniedMcpServers), so a change there never reaches Opt. The claude.ai
// connectors and plugins' servers come as before: they can't be copied,
// and --strict-mcp-config would drop them too.

// mcpFile is an MCP config file: Opt's, or a folder's .mcp.json.
type mcpFile struct {
	Servers map[string]jsontext.Value `json:"mcpServers"`
}

// ccConfig is what Claude Code's config says of MCP servers: the user's,
// and each folder's own (projects, by path).
type ccConfig struct {
	Servers  map[string]jsontext.Value `json:"mcpServers"`
	Projects map[string]struct {
		Servers  map[string]jsontext.Value `json:"mcpServers"`
		Disabled []string                  `json:"disabledMcpServers"`
	} `json:"projects"`
}

// ccConfigPath is Claude Code's config file.
func ccConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(cmp.Or(os.Getenv("CLAUDE_CONFIG_DIR"), home), ".claude.json")
}

// readJSON reads path into v; a missing file leaves v as it is.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// seedOptMCP makes Opt's file, if it has none, a copy of the servers Claude
// Code gives the folder from, where Opt worked until now: the user's and
// the folder's own, which win on a name. Those Claude Code keeps off there
// go on Opt's list of servers off (settings), unless that list or the one
// for subagents has them, so none comes on.
func seedOptMCP(dir, claudeJSON, from string) error {
	dst := filepath.Join(dir, "mcp.json")
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		return err // nil: there already, and Opt's
	}
	var cc ccConfig
	if err := readJSON(claudeJSON, &cc); err != nil {
		return err
	}
	servers := map[string]jsontext.Value{}
	maps.Copy(servers, cc.Servers)
	var off []string
	for _, key := range sameDirs(from) {
		p := cc.Projects[key]
		maps.Copy(servers, p.Servers)
		off = append(off, p.Disabled...)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Settings first: with the file there, they would never be added.
	s := loadSettings(dir)
	n := len(s.MCPOff)
	for _, name := range off {
		if !slices.Contains(s.MCPOff, name) && !slices.Contains(s.MCPAgents, name) {
			s.MCPOff = append(s.MCPOff, name)
		}
	}
	if len(s.MCPOff) > n {
		if err := saveSettings(dir, s); err != nil {
			return err
		}
	}
	b, err := json.Marshal(mcpFile{servers}, jsontext.Multiline(true), json.Deterministic(true))
	if err != nil {
		return err
	}
	return writeAtomic(dst, append(b, '\n')) // 0600: it holds keys
}

// sameDirs is dir and, if another, its real path: Claude Code's config
// names a folder by either.
func sameDirs(dir string) []string {
	out := []string{dir}
	if real, err := filepath.EvalSymlinks(dir); err == nil && real != dir {
		out = append(out, real)
	}
	return out
}

// ccDenied is the servers of Claude Code's own that a call in cwd would get
// and Opt's file (optMCP) doesn't name, sorted: the user's, and those of
// cwd and the folders above it, in Claude Code's config or a .mcp.json.
// ok tells whether Opt's file is there, for the call to load.
func ccDenied(claudeJSON, cwd, optMCP string) (deny []string, ok bool) {
	var opt mcpFile
	b, err := os.ReadFile(optMCP)
	if err == nil {
		if err := json.Unmarshal(b, &opt); err != nil {
			warn("%s: %v", optMCP, err) // Claude Code would refuse it, and the call
		} else {
			ok = true
		}
	}
	var cc ccConfig
	readJSON(claudeJSON, &cc) // one it can't read, Claude Code says so
	names := slices.Collect(maps.Keys(cc.Servers))
	for _, start := range sameDirs(cwd) {
		for dir := start; ; dir = filepath.Dir(dir) {
			names = slices.AppendSeq(names, maps.Keys(cc.Projects[dir].Servers))
			var f mcpFile
			readJSON(filepath.Join(dir, ".mcp.json"), &f)
			names = slices.AppendSeq(names, maps.Keys(f.Servers))
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	for _, name := range names {
		if _, mine := opt.Servers[name]; !mine && name != "opt" { // "opt": the chat's own, which no deny may reach
			deny = append(deny, name)
		}
	}
	slices.Sort(deny)
	return slices.Compact(deny), ok
}

// mcpPrompt is the prompt's paragraph on Opt's own servers, there only with
// them.
const mcpPrompt = `Your MCP servers, but for claude.ai's connectors and plugins', are your own,
in {{mcp.json}}, in Claude Code's format ({"mcpServers": {"name": {...}}}); the
user's Claude Code servers are kept out of your calls. To add, change or drop
one, as the user asks, edit that file: each call loads it as it starts.
`
