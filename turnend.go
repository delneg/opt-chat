package main

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A turn that stops short says why in the chat, so that Opt knows at its
// next turn, as UniiChat's notes do (Server.ts: "Stopped by the user: ...",
// "No reply: <why>. Send a message to try again."): a stop, a failure, or
// a quit or crash that cut it off. A turn underway is kept in turn.json;
// a start that finds it there resumes it, the way UniiChat's resume goes
// on after a restart ("The server restarted, cutting Unii's turn; nothing
// was lost: go on."), and as resumeCut does for subagents.

// turnRecord is the turn underway, as turn.json keeps it.
type turnRecord struct {
	State   string `json:"state"`            // running; quit: a quit cut it off
	Resumes int    `json:"resumes,omitzero"` // times a start resumed it since a turn last ended
}

func (a *Agent) turnPath() string { return filepath.Join(a.chat.dir, "turn.json") }

func (a *Agent) loadTurn() (turnRecord, bool) {
	var r turnRecord
	b, err := os.ReadFile(a.turnPath())
	return r, err == nil && json.Unmarshal(b, &r) == nil
}

func (a *Agent) saveTurn(r turnRecord) {
	b, err := json.Marshal(r)
	if err == nil {
		err = writeAtomic(a.turnPath(), b)
	}
	if err != nil {
		warn("turn record: %v", err)
	}
}

// turnBegan records a turn underway: a crash leaves it so.
func (a *Agent) turnBegan() {
	r, _ := a.loadTurn() // a resumed turn's: its count goes on
	r.State = "running"
	a.saveTurn(r)
}

// turnEnded records the turn's end: a quit cut it off, for the next start
// to resume, or it is over.
func (a *Agent) turnEnded(quit bool) {
	if !quit {
		if err := os.Remove(a.turnPath()); err != nil && !os.IsNotExist(err) {
			warn("turn record: %v", err)
		}
		return
	}
	r, _ := a.loadTurn()
	r.State = "quit"
	a.saveTurn(r)
}

// endNote is what the chat says of a turn that ended short, in a way Opt
// reads at its next turn; "" when it ended as it should, or a quit cut it
// off (the next start says so). cut: its call was stopped; stopped: by the
// user's stop; else by a message they sent at once.
func endNote(err error, cut, stopped, quitting bool) string {
	switch {
	case quitting:
		return ""
	case cut && stopped:
		return "[opt-chat] Stopped by the user."
	case cut:
		return "[opt-chat] Stopped by the user, who sent the next message at once."
	case err != nil:
		return "[opt-chat] No reply: " + strings.TrimSuffix(oneLine(err.Error(), 300), ".") + ". Send a message to try again."
	}
	return ""
}

// recoverTurn, as opt-chat starts, finds the turn a quit or a crash cut
// off, and gives the note that says so; resume: start a turn on it. One is
// resumed while -auto-resume is on, under a day after it was cut off, and
// fewer than resumeMax times in a row: one that ends opt-chat each time
// stops. A quit with no turn underway leaves none.
func (a *Agent) recoverTurn() (note string, resume bool) {
	r, ok := a.loadTurn()
	if !ok {
		os.Remove(a.turnPath())
		return "", false
	}
	fi, err := os.Stat(a.turnPath())
	if err != nil || (r.State != "running" && r.State != "quit") {
		os.Remove(a.turnPath())
		return "", false
	}
	how := "quit"
	if r.State == "running" {
		how = "ended without quitting (a crash?)"
	}
	why := ""
	switch {
	case !a.autoResume:
	case r.Resumes >= resumeMax:
		why = fmt.Sprintf(" opt-chat resumed it %d times in a row, so not again on its own.", resumeMax)
	case time.Since(fi.ModTime()) >= 24*time.Hour:
		why = " That was over a day ago, so opt-chat did not resume it on its own."
	}
	if !a.autoResume || why != "" {
		os.Remove(a.turnPath())
		return fmt.Sprintf("[opt-chat] opt-chat %s while Opt's turn was underway, cutting it off.%s", how, why), false
	}
	a.saveTurn(turnRecord{State: "resumed", Resumes: r.Resumes + 1})
	return fmt.Sprintf("[opt-chat] opt-chat %s while Opt's turn was underway, cutting it off, and has started again: nothing was lost, go on with that turn. %s", how, cutShort), true
}
