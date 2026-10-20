package main

import (
	"os"
	"path/filepath"
)

// Claude Code adds a context note to each call's first message: the
// account's email ("# userEmail\nThe user's email address is ..."), from
// the login it keeps in ~/.claude.json. No flag, setting or variable turns
// it off. Compactions read it as part of the chat and copied it into the
// tree's lines (found by https://github.com/abhaymettu/UniiChat-claude-cli).
//
// A plugin turns it off: its prompt.context hook, Claude Code's own hook on
// the blocks of that note, drops the email's. opt-chat writes the plugin to
// <dir>/claude-plugin and loads it with --plugin-dir into the calls that
// get none of the user's setup: compactions always, and turns, subagents
// and side questions under -claude-config=false. The others run with
// disableAllHooks, which keeps the user's hooks out and turns off every
// plugin's hooks with them, this one too: the prompt tells compactions
// never to copy the note.

// emailPlugin is the plugin's files, by path in its folder.
var emailPlugin = map[string]string{
	".claude-plugin/plugin.json": `{ "name": "opt-chat-context", "version": "0.1.0", "description": "Drops the account's email from the context Claude Code adds to opt-chat's calls" }
`,
	"hooks/hooks.json": `{ "modules": ["./register.ts"] }
`,
	"hooks/register.ts": `import type { Register } from 'claude-code'

export const register: Register = on => {
  on('prompt.context', async ($, e, next) => {
    const r = await next(e)
    return { ...r, blocks: r.blocks.filter(b => b.name !== 'userEmail') }
  })
}
`,
}

// writeEmailPlugin writes the plugin into dir/claude-plugin, each file only
// when it changed, and gives its folder.
func writeEmailPlugin(dir string) (string, error) {
	root := filepath.Join(dir, "claude-plugin")
	for name, body := range emailPlugin {
		path := filepath.Join(root, name)
		if b, err := os.ReadFile(path); err == nil && string(b) == body {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		if err := writeAtomic(path, []byte(body)); err != nil {
			return "", err
		}
	}
	return root, nil
}
