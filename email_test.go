package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The plugin is written once, and loaded into the calls that run without
// the user's setup; with it, disableAllHooks would turn it off.
func TestEmailPlugin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := writeEmailPlugin(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "hooks", "register.ts"))
	if err != nil || !strings.Contains(string(b), "'prompt.context'") || !strings.Contains(string(b), "b.name !== 'userEmail'") {
		t.Fatalf("hook: %s, %v", b, err)
	}
	manifest := filepath.Join(root, ".claude-plugin", "plugin.json")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	os.Chtimes(manifest, old, old)
	if _, err := writeEmailPlugin(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(manifest); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("written again unchanged: %v", err)
	}

	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	a, calls := fakeAgent(t, c)
	a.noEmail = root
	a.Compact(context.Background(), []string{"x"}, 1, []block{textBlock("Compaction: x")})
	a.ownConfig = true
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	a.ownConfig = false
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	all := calls()
	if flagOf(all[0], "--plugin-dir") != root || slices.Contains(all[1], "--plugin-dir") || flagOf(all[2], "--plugin-dir") != root {
		t.Fatalf("calls: %q", all)
	}
}
