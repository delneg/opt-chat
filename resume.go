package main

import (
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A subagent's call is a Claude Code session saved to disk (the chat's own
// calls are not), so one that opt-chat's quit, a crash or the end of its
// rest stopped goes on where it stopped: a tell resumes its session, with
// all it knew. agents/<Name>.json keeps what that takes.

// agentRecord is a subagent's, as agents/<Name>.json keeps it.
type agentRecord struct {
	Session   string    `json:"session"` // Claude Code's session: --resume goes on from it
	Model     string    `json:"model"`
	Effort    string    `json:"effort"`
	Window    int       `json:"window,omitzero"`
	Task      string    `json:"task"`
	State     string    `json:"state"`              // working, resting, ended, stopped, failed
	Compacted bool      `json:"compacted,omitzero"` // since its last message: the next brings the view and its task again
	Loop      *loopHook `json:"loop,omitzero"`      // its Stop hook, run at the end of each turn: see loop.go
}

func (a *Agent) recordPath(name string) string {
	return filepath.Join(a.chat.dir, "agents", name+".json")
}

func (a *Agent) loadRecord(name string) (agentRecord, error) {
	var r agentRecord
	b, err := os.ReadFile(a.recordPath(name))
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}

// note changes the subagent name's record.
func (a *Agent) note(name string, f func(*agentRecord)) {
	a.recMu.Lock()
	defer a.recMu.Unlock()
	r, _ := a.loadRecord(name)
	f(&r)
	b, err := json.Marshal(r)
	if err == nil {
		err = writeAtomic(a.recordPath(name), b)
	}
	if err != nil {
		warn("[%s] record: %v", name, err)
	}
}

func newSession() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40 // version 4
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// saved tells whether Claude Code has the session on disk, where it keeps
// every project's: --resume of one it has not starts afresh, silently.
func saved(session string) bool { return sessionPath(session) != "" }

// sessionPath is where Claude Code keeps the session, or "".
func sessionPath(session string) string {
	found, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", session+".jsonl"))
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// resumeNote begins the message that resumes a subagent's session, its
// record in state: one whose call ended after its report, or one stopped
// mid-task.
func resumeNote(state string) string {
	if state == "ended" || state == "resting" { // resting: its call ended before the record said so
		return "Your call had ended after your report; opt-chat resumed it from its saved session. Opt's message:\n"
	}
	if state == "halted" {
		return "The user stopped you while you worked, and opt-chat has now resumed your call from its saved session: a tool call underway then was cut short. Opt's message:\n"
	}
	return "opt-chat stopped while you worked, and resumed your call from its saved session: a tool call underway then was cut short. Opt's message:\n"
}

// recoverAgents, as opt-chat starts, marks the subagents a crash stopped
// mid-task, and returns a report for each, for the chat; those stopped in
// the last day, by a crash, a quit or the user, show in the rail.
func (a *Agent) recoverAgents() []message {
	files, _ := filepath.Glob(filepath.Join(a.chat.dir, "agents", "*.json"))
	var out []message
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		r, err := a.loadRecord(name)
		fi, serr := os.Stat(f)
		if err != nil || serr != nil {
			continue
		}
		if r.State == "resting" { // its call ended with opt-chat: nothing lost
			a.note(name, func(r *agentRecord) { r.State = "ended" })
		}
		if r.State == "working" {
			a.note(name, func(r *agentRecord) { r.State = "stopped" })
			r.State = "stopped"
			out = append(out, message{kind: "work", text: fmt.Sprintf("[%s] stopped: opt-chat ended without quitting (a crash?) before this subagent finished. %s\n\nRan on %s. Full chat: zoom(%q)",
				name, resumeHint(name), strings.TrimSpace(r.Model+" "+r.Effort), name)})
		}
		if (r.State == "stopped" || r.State == "halted") && time.Since(fi.ModTime()) < 24*time.Hour {
			a.setSub(name, func(s *sub) {
				*s = sub{Name: name, Model: r.Model, Effort: r.Effort, Window: r.Window, State: r.State, Started: fi.ModTime().UnixMilli()}
			})
		}
	}
	return out
}

func resumeHint(name string) string {
	return fmt.Sprintf("tell(%q, message) resumes it where it stopped, with all it knew.", name)
}
