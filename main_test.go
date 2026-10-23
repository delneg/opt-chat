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
	msg = "run `cat " + a + "` (" + notes + ") [the pdf](~/h.pdf)\u00a0and\n```\n" + spaced + "\n```\n  ~~~\n" + quoted + "\n  ~~~\n"
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
	a := &Agent{chat: &Chat{dir: "/chat"}, model: "fable", effort: "high", perm: "auto", running: map[string]*proc{}}
	for _, step := range []struct{ in, out string }{
		{"/model", "Turns: fable, effort high, mode auto."},
		{" /model opus ", "Turns: opus, effort high, mode auto."},
		{"/model opus please", "a model is one word, as for -model: fable, opus, sonnet, or a full name"},
		{"/effort low", "Turns: opus, effort low, mode auto."},
		{"/effort lots", "effort must be one of low, medium, high, xhigh, max"},
		{"/effort default", "effort must be one of low, medium, high, xhigh, max"},
		{"/EFFORT max", "Turns: opus, effort max, mode auto."},
		{"/mode manual", "Turns: opus, effort max, mode manual."},
		{"/mode sideways", "\"sideways\" is no permission mode: manual, acceptEdits, auto"},
		{"/agents", "No subagent is at work."},
		{"/fix this\nand that", "/fix is no command; nothing was sent. /help lists the commands"},
		{"/review-simple", "/review-simple is no command; nothing was sent. /help lists the commands"},
	} {
		out, ok, err := command(a, step.in)
		if err != nil {
			out = err.Error()
		}
		if !ok || out != step.out {
			t.Fatalf("%q: %q, %v; want %q", step.in, out, ok, step.out)
		}
	}
	a.running["Scout"], a.running["Ann"] = nil, nil
	if out, _, _ := command(a, "/agents"); !strings.HasPrefix(out, "At work: [Ann], [Scout]. ") {
		t.Fatal(out)
	}
	if out, _, _ := command(a, "/help"); !strings.Contains(out, "/effort [level]") {
		t.Fatal(out)
	}
	for _, msg := range []string{"/Users/me/shot.png what is it?", "hi /model", "/ x", "/12"} {
		if out, ok, err := command(a, msg); ok || err != nil {
			t.Fatalf("%q taken as a command: %q, %v", msg, out, err)
		}
	}
}

// A /name of one of the user's skills goes to Opt, for Claude Code to run
// the skill; one turned off for Opt is held back, as a typo is.
func TestSkillCommand(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	a.ownConfig = true
	if !asks(a, "/one") || !asks(a, "/mcp") || asks(a, "/model x") || asks(a, "hi") {
		t.Fatal("asks: not as listed")
	}
	for _, msg := range []string{"/one", "/one look at main.go", "/plug:two"} {
		if out, ok, err := command(a, msg); ok || err != nil {
			t.Fatalf("%q taken as a command: %q, %v", msg, out, err)
		}
	}
	if asks(a, "/one x") || !asks(a, "/three") { // listed now: answered at once; another is asked
		t.Fatal("asks: the listing not kept")
	}
	a.skillsSet = map[string]string{"one": "off"}
	if _, ok, err := command(a, "/one x"); !ok || err == nil || err.Error() != "/one is a skill turned off for Opt; nothing was sent. /skills turns it on" {
		t.Fatal(ok, err)
	}
	if _, ok, err := command(a, "/One"); !ok || err == nil {
		t.Fatal("a skill's name taken in another case:", ok, err)
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

// The system prompt names the chat's AGENTS.md, and lists wake only when on.
func TestSystemPrompt(t *testing.T) {
	path := "/chats/x/AGENTS.md"
	off := systemPrompt(path, false, false)
	if strings.Contains(off, "{{") || !strings.Contains(off, path) || strings.Contains(off, "wake(") ||
		!strings.Contains(off, "unchecked") || !strings.Contains(off, "CLAUDE.md") {
		t.Fatalf("prompt without wake:\n%s", off)
	}
	on := systemPrompt(path, true, false)
	if strings.Count(on, "- wake(") != 1 || strings.Index(on, "- wake(") > strings.Index(on, "# Turns") || strings.Index(on, "- wake(") < strings.Index(on, "- tell(") {
		t.Fatalf("prompt with wake:\n%s", on)
	}
}
