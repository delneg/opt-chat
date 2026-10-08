package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// imagePaths reads paths the way terminals drop them.
func TestImagePaths(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.png", "my shot.JPG", "q'd.gif"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	os.Mkdir(filepath.Join(dir, "d.png"), 0o700)
	a, spaced, quoted := filepath.Join(dir, "a.png"), filepath.Join(dir, "my shot.JPG"), filepath.Join(dir, "q'd.gif")
	msg := "see " + a + ", " + dir + `/my\ shot.JPG and '` + spaced + `' "` + quoted + `" ` + dir + "/d.png " + dir + "/none.png notes.txt"
	if got := imagePaths(msg); !slices.Equal(got, []string{a, spaced, spaced, quoted}) {
		t.Fatalf("imagePaths = %q", got)
	}
}

// Commands change the turns' model and effort; any other /word is held back.
func TestCommand(t *testing.T) {
	a := &Agent{chat: &Chat{dir: "/chat"}, model: "fable", running: map[string]*proc{}}
	for _, step := range []struct{ in, out string }{
		{"/model", "Turns: fable, effort default."},
		{" /model opus ", "Turns: opus, effort default."},
		{"/model opus please", "A model is one word, as for -model: fable, opus, sonnet, or a full name."},
		{"/effort high", "Turns: opus, effort high."},
		{"/effort lots", "effort must be one of low, medium, high, xhigh, max, or default."},
		{"/EFFORT default", "Turns: opus, effort default."},
		{"/agents", "No subagent is at work."},
		{"/fix this\nand that", "/fix is no command; nothing was sent. /help lists the commands."},
	} {
		if out, ok := command(a, step.in); !ok || out != step.out {
			t.Fatalf("%q: %q, %v; want %q", step.in, out, ok, step.out)
		}
	}
	a.running["Scout"], a.running["Ann"] = nil, nil
	if out, _ := command(a, "/agents"); !strings.HasPrefix(out, "At work: [Ann], [Scout]. ") {
		t.Fatal(out)
	}
	if out, _ := command(a, "/help"); !strings.Contains(out, "/effort [level]") {
		t.Fatal(out)
	}
	for _, msg := range []string{"/Users/me/shot.png what is it?", "hi /model", "/ x", "/12"} {
		if out, ok := command(a, msg); ok {
			t.Fatalf("%q taken as a command: %q", msg, out)
		}
	}
}
