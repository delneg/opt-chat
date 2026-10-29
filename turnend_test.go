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
	t.Parallel()
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
	t.Parallel()
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
	if !resume || !strings.HasPrefix(note, "[opt-chat] Quit while Opt's turn was underway") {
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
	if note, resume := a.recoverTurn(); !resume || !strings.Contains(note, "Ended without quitting (a crash?)") {
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
	if note, resume := a.recoverTurn(); resume || note != "[opt-chat] Quit while Opt's turn was underway, cutting it off." {
		t.Fatalf("with -auto-resume off: %q, %v", note, resume)
	}
	a.autoResume = true
	a.saveTurn(turnRecord{State: "resumed", Resumes: 1}) // a crash before the resumed turn began
	if note, resume := a.recoverTurn(); !resume || !strings.Contains(note, "Ended without quitting (a crash?)") {
		t.Fatalf("a crash before a resumed turn began: %q, %v", note, resume)
	}
	if rec, _ := a.loadTurn(); rec.State != "resumed" || rec.Resumes != 2 {
		t.Fatalf("resumes counted: %+v", rec)
	}
	a.turnEnded(false)
	if note, resume := a.recoverTurn(); note != "" || resume {
		t.Fatalf("a quit when idle: %q, %v", note, resume)
	}
}

// A failed result and then a good one in the same call (a message sent
// mid-run) end the turn well: no note that it failed.
func TestTurnRecovers(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "fail-then-ok"})
	waitFor(t, "the reply", func() bool { return hasLogged(c, "opt: recovered") })
	waitFor(t, "the loop to end", h.idle)
	if slices.ContainsFunc(logged(c), func(l string) bool { return strings.Contains(l, "No reply") }) {
		t.Fatalf("logged %q", logged(c))
	}
}

// Esc, send now or a quit while the call exits after its answer cut
// nothing: no note, and no turn to resume.
func TestStopAfterAnswer(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"stop", "send now", "quit"} {
		c, _ := fakeChat(t, t.TempDir(), 100)
		r := record(c)
		h := newHarness(t, c)
		h.input(message{kind: "user", text: "answer-then-linger"})
		waitFor(t, "the answer", func() bool {
			if !hasLogged(c, "opt: answered") {
				return false
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			h.turn.mu.Lock()
			defer h.turn.mu.Unlock()
			return !h.turn.open // its result is in
		})
		switch how {
		case "stop":
			h.stop()
		case "send now":
			h.input(message{kind: "user", text: "next"})
			h.sendNow()
			waitFor(t, "the next turn", func() bool {
				return slices.ContainsFunc(logged(c), func(s string) bool { return strings.HasPrefix(s, "opt: answer to") })
			})
		case "quit":
			h.quit()
		}
		waitFor(t, "the loop to end", h.idle)
		if slices.ContainsFunc(logged(c), func(s string) bool { return strings.HasPrefix(s, "work: [opt-chat]") }) {
			t.Fatalf("%s after the answer: logged %q", how, logged(c))
		}
		if _, err := os.Stat(h.agent.turnPath()); !os.IsNotExist(err) {
			t.Fatalf("%s after the answer left a turn record: %v", how, err)
		}
		if w := r.warnings(); len(w) > 0 {
			t.Fatalf("%s after the answer warned: %q", how, w)
		}
	}
}

// Esc then a quick Ctrl-C: the turn was stopped, as its note says, and the
// next start has none to resume.
func TestStopThenQuit(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "wait-for-stop"})
	waitFor(t, "the turn", func() bool { return hasLogged(c, "opt: working") })
	h.stop()
	h.quit()
	if !hasLogged(c, "work: [opt-chat] Stopped by the user.") {
		t.Fatalf("logged %q", logged(c))
	}
	if _, err := os.Stat(h.agent.turnPath()); !os.IsNotExist(err) {
		t.Fatalf("a stopped turn left its record: %v", err)
	}
}

// A quit before a resumed turn began, as it waits for its summaries, leaves
// it for the next start, its resumes counted; a stop ends it.
func TestResumedTurnUnbegun(t *testing.T) {
	t.Parallel()
	for _, quit := range []bool{true, false} {
		c, _ := fakeChat(t, t.TempDir(), 100)
		building(t, c) // nothing builds till the end: the turn waits
		c.Log("note", strings.Repeat("n", 700))
		record(c)
		h := newHarness(t, c)
		h.agent.saveTurn(turnRecord{State: "resumed", Resumes: 1})
		h.input(message{kind: "work", text: "[opt-chat] Quit while Opt's turn was underway"})
		waitFor(t, "the wait for summaries", func() bool {
			lt, _ := hubState(c)
			return lt.Phase == "settling"
		})
		if quit {
			h.quit()
			if rec, ok := h.agent.loadTurn(); !ok || rec.State != "quit" || rec.Resumes != 1 {
				t.Fatalf("a quit before a resumed turn began: %+v, %v", rec, ok)
			}
			continue
		}
		h.stop()
		waitFor(t, "the loop to end", h.idle)
		if _, err := os.Stat(h.agent.turnPath()); !os.IsNotExist(err) {
			t.Fatalf("a stop before a resumed turn began left its record: %v", err)
		}
	}
}
