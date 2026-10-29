package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The @ menu: a folder's entries that start with the last part first, then
// those that contain it, hidden ones only when asked; a name alone also
// finds files deeper down, never in node_modules.
func TestFileSuggestions(t *testing.T) {
	t.Parallel()
	cwd := t.TempDir()
	for _, p := range []string{"src/app.js", "src/lib/apple.txt", "apps/x", "map.txt", ".secret", "node_modules/app/index.js"} {
		os.MkdirAll(filepath.Dir(filepath.Join(cwd, p)), 0o700)
		os.WriteFile(filepath.Join(cwd, p), nil, 0o600)
	}
	paths := func(q string) []string {
		var out []string
		for _, s := range fileSuggestions(cwd, q) {
			out = append(out, s.Path)
		}
		return out
	}
	abs := func(p string) string { return filepath.Join(cwd, p) }
	for _, step := range []struct {
		q    string
		want []string
	}{
		{"ap", []string{abs("apps") + "/", abs("map.txt")}},
		{"src/", []string{abs("src/app.js"), abs("src/lib") + "/"}},
		{"src/AP", []string{abs("src/app.js")}},
		{".", []string{abs(".secret")}},
		{"app", []string{abs("apps") + "/", abs("src/app.js"), abs("src/lib/apple.txt")}},
		{"nothing-like-it", nil},
	} {
		if got := paths(step.q); !slices.Equal(got, step.want) {
			t.Fatalf("%q: %q, want %q", step.q, got, step.want)
		}
	}
	home, _ := os.UserHomeDir()
	if got := fileSuggestions(cwd, "~/"); len(got) == 0 || got[0].Path[:2] != "~/" {
		t.Fatalf("~/: %v (home %s)", got, home)
	}
}
