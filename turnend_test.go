package main

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A failed turn and a stopped one each leave a note Opt reads at its next
// turn, and neither leaves a turn to resume.
func TestTurnEndNotes(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "side-fail"})
	waitFor(t, "the failed turn", func() bool {
		return hasLogged(c, "work: [opt-chat] No reply: overloaded. Send a message to try again.")
	})
	waitFor(t, "the loop to end", h.idle)
	if w := r.warnings(); !slices.Equal(w, []string{"turn: overloaded"}) {
		t.Fatalf("warnings %q", w)
	}
	h.input(message{kind: "user", text: "wait-for-stop"})
	waitFor(t, "the turn", func() bool { return hasLogged(c, "opt: working") })
	if rec, ok := h.agent.loadTurn(); !ok || rec.State != "running" {
		t.Fatalf("a turn underway: %+v, %v", rec, ok)
	}
	h.stop()
	waitFor(t, "the turn to stop", h.idle)
	if !hasLogged(c, "work: [opt-chat] Stopped by the user.") {
		t.Fatalf("logged %q", logged(c))
	}
	if _, err := os.Stat(h.agent.turnPath()); !os.IsNotExist(err) {
		t.Fatalf("a stopped turn left its record: %v", err)
	}
	if note, resume := h.agent.recoverTurn(); note != "" || resume {
		t.Fatalf("a start after a stop: %q, %v", note, resume)
	}
}

// A quit mid-turn leaves it for the next start, which says so and resumes
// it; so does a crash. One resumed too often, or too long ago, or with
// -auto-resume off, gets the note alone. A quit when idle leaves nothing.
func TestTurnCut(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "wait-for-stop"})
	waitFor(t, "the turn", func() bool { return hasLogged(c, "opt: working") })
	h.quit()
	if rec, _ := h.agent.loadTurn(); rec.State != "quit" {
		t.Fatalf("after a quit mid-turn: %+v", rec)
	}
	if slices.ContainsFunc(logged(c), func(s string) bool { return strings.HasPrefix(s, "work: [opt-chat] Stopped") }) {
		t.Fatalf("a quit read as a stop: %q", logged(c))
	}

	h2 := newHarness(t, c) // the next start
	a := h2.agent
	a.autoResume = true
	note, resume := a.recoverTurn()
	if !resume || !strings.HasPrefix(note, "[opt-chat] opt-chat quit while Opt's turn was underway") {
		t.Fatalf("after a quit: %q, %v", note, resume)
	}
	h2.input(message{kind: "work", text: note})
	waitFor(t, "the resumed turn", func() bool {
		return slices.ContainsFunc(logged(c), func(s string) bool {
			return strings.HasPrefix(s, "opt: answer to") && strings.Contains(s, "go on with that turn")
		})
	})
	waitFor(t, "the loop to end", h2.idle)
	if _, err := os.Stat(a.turnPath()); !os.IsNotExist(err) {
		t.Fatalf("a resumed turn that ended left its record: %v", err)
	}

	a.saveTurn(turnRecord{State: "running", Resumes: 1}) // a crash
	if note, resume := a.recoverTurn(); !resume || !strings.Contains(note, "ended without quitting (a crash?)") {
		t.Fatalf("after a crash: %q, %v", note, resume)
	}
	if rec, _ := a.loadTurn(); rec.Resumes != 2 {
		t.Fatalf("resumes counted: %+v", rec)
	}
	a.saveTurn(turnRecord{State: "running", Resumes: resumeMax})
	if note, resume := a.recoverTurn(); resume || !strings.Contains(note, "3 times in a row") {
		t.Fatalf("resumed too often: %q, %v", note, resume)
	}
	a.saveTurn(turnRecord{State: "quit"})
	old := time.Now().Add(-25 * time.Hour)
	os.Chtimes(a.turnPath(), old, old)
	if note, resume := a.recoverTurn(); resume || !strings.Contains(note, "over a day ago") {
		t.Fatalf("cut a day ago: %q, %v", note, resume)
	}
	a.autoResume = false
	a.saveTurn(turnRecord{State: "quit"})
	if note, resume := a.recoverTurn(); resume || note != "[opt-chat] opt-chat quit while Opt's turn was underway, cutting it off." {
		t.Fatalf("with -auto-resume off: %q, %v", note, resume)
	}
	a.saveTurn(turnRecord{State: "resumed", Resumes: 1}) // quit before the resumed turn began: its note is in the chat
	if note, resume := a.recoverTurn(); note != "" || resume {
		t.Fatalf("a resume that never began: %q, %v", note, resume)
	}
	if note, resume := a.recoverTurn(); note != "" || resume {
		t.Fatalf("a quit when idle: %q, %v", note, resume)
	}
}
