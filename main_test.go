package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// filePaths reads paths the way terminals drop them: absolute or from ~/,
// files only, each once.
func TestFilePaths(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"a.png", "my shot.JPG", "q'd.gif", "notes.txt"} {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
	}
	os.Mkdir(filepath.Join(dir, "d.png"), 0o700)
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.WriteFile(filepath.Join(home, "h.pdf"), []byte("x"), 0o600)
	a, spaced, quoted, notes := filepath.Join(dir, "a.png"), filepath.Join(dir, "my shot.JPG"), filepath.Join(dir, "q'd.gif"), filepath.Join(dir, "notes.txt")
	t.Chdir(dir)
	msg := "see " + a + ", " + dir + `/my\ shot.JPG and '` + spaced + `' "` + quoted + `" ` + dir + "/d.png " + dir + "/none.png notes.txt " + notes + ". ~/h.pdf"
	if got := filePaths(msg); !slices.Equal(got, []string{a, spaced, quoted, notes, filepath.Join(home, "h.pdf")}) {
		t.Fatalf("filePaths = %q", got)
	}
	// In code, brackets and links; not in a block of code.
	msg = "run `cat " + a + "` (" + notes + ") [the pdf](~/h.pdf)\u00a0and\n```\n" + spaced + "\n```\n"
	if got := filePaths(msg); !slices.Equal(got, []string{a, notes, filepath.Join(home, "h.pdf")}) {
		t.Fatalf("filePaths = %q", got)
	}
	// Many paths are a listing, not files to send.
	if got := filePaths(strings.Repeat(a+" "+notes+" "+filepath.Join(home, "h.pdf")+" ", 4)); len(got) != 3 {
		t.Fatalf("each named once: %q", got)
	}
	var many []string
	for i := range pathsMax + 1 {
		p := filepath.Join(dir, fmt.Sprint(i, ".txt"))
		os.WriteFile(p, []byte("x"), 0o600)
		many = append(many, p)
	}
	if got := filePaths(strings.Join(many, "\n")); got != nil {
		t.Fatalf("a listing attached %d files", len(got))
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

// In the terminal, as in the page, naming a hidden file does not send it.
func TestTypedHidden(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	dir := t.TempDir()
	plain, secret := filepath.Join(dir, "plain.txt"), filepath.Join(dir, ".env")
	os.WriteFile(plain, []byte("PLAIN"), 0o600)
	os.WriteFile(secret, []byte("SECRET"), 0o600)
	m := typed(c, "see "+plain+" and "+secret)
	if full := m.full(); !strings.Contains(full, "PLAIN") || strings.Contains(full, "SECRET") {
		t.Fatalf("message:\n%s", full)
	}
	if !slices.ContainsFunc(r.notices(), func(s string) bool { return strings.Contains(s, ".env is hidden") }) {
		t.Fatalf("notices: %q", r.notices())
	}
}
