package main

import (
	"crypto/rand"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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
	Turns     int       `json:"turns,omitzero"`     // its loop's turns since its last report: the cap holds across a restart
	Cut       bool      `json:"cut,omitzero"`       // cut off mid-task by a quit, a crash or a reload: the next start resumes it
	Resumes   int       `json:"resumes,omitzero"`   // times a start resumed it since its last result
	Pid       int       `json:"pid,omitzero"`       // its Claude Code's, while at work: after a crash, stopped before it is resumed
	User      bool      `json:"user,omitzero"`      // its turn underway is the user's, from the page: its end reports nothing to Opt
	MCP       []string  `json:"mcp,omitzero"`       // the servers for subagents only granted to it
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
// mid-task. Opt's message or the user's follows, with its head.
func resumeNote(state string) string {
	if state == "ended" || state == "resting" { // resting: its call ended before the record said so
		return "Your call had ended after your report; opt-chat resumed it from its saved session. "
	}
	if state == "halted" {
		return "The user stopped you while you worked, and opt-chat has now resumed your call from its saved session. " + cutShort + " "
	}
	return "opt-chat stopped while you worked, and resumed your call from its saved session. " + cutShort + " "
}

const cutShort = "Your last tool call may have been cut off: check its effect before redoing it."

// restartNote is the whole message that resumes a subagent cut off
// mid-task, as opt-chat starts again: see resumeCut. restartUserNote is
// for one cut off as it answered the user's message.
const (
	restartNote = "opt-chat restarted while you worked, and resumed your call from its saved session on its own: Opt sent no message. " +
		cutShort + " Then go on with your task."
	restartUserNote = "opt-chat restarted while you worked, and resumed your call from its saved session on its own: no one sent a message. " +
		cutShort + " Then go on with what the user asked."
)

// resumeMax is how many times in a row a start resumes a subagent that
// gets no result in between: one that ends opt-chat each time stops.
const resumeMax = 3

// recoverAgents, as opt-chat starts, lists the subagents to resume: each
// one a quit, a crash or a reload cut off mid-task, under a day ago, and
// resumed fewer than resumeMax times since its last result, while
// auto-resume is on. For each other one cut off, it gives a report, for
// the chat. Those stopped in the last day, by a crash, a quit or the user,
// show in the rail.
func (a *Agent) recoverAgents() (out []message, cut []string) {
	files, _ := filepath.Glob(filepath.Join(a.chat.dir, "agents", "*.json"))
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
		if r.State == "working" || r.Cut {
			at := fi.ModTime() // as it was cut off: its record's last change, or its last step
			if li, err := os.Stat(a.agentLog(name)); err == nil && li.ModTime().After(at) {
				at = li.ModTime()
			}
			why := ""
			switch {
			case !a.autoResume:
			case r.Resumes >= resumeMax:
				why = fmt.Sprintf(" opt-chat resumed it %d times since its last result, so not again on its own.", resumeMax)
			case time.Since(at) >= 24*time.Hour:
				why = " That was over a day ago, so opt-chat did not resume it on its own."
			}
			resume, crash := a.autoResume && why == "", r.State == "working"
			if crash && r.Pid > 0 {
				stopOrphan(r.Pid, r.Session)
			}
			a.note(name, func(r *agentRecord) { r.State, r.Cut = "stopped", resume })
			r.State = "stopped"
			how := "quit"
			if crash {
				how = "ended without quitting (a crash?)"
			}
			switch {
			case resume:
				cut = append(cut, name)
			case r.User: // the user's turn: Opt never hears of it
			default:
				out = append(out, message{kind: "work", text: fmt.Sprintf("[%s] stopped: opt-chat %s before this subagent finished.%s %s\n\nRan on %s. Full chat: zoom(%q)",
					name, how, why, resumeHint(name), strings.TrimSpace(r.Model+" "+r.Effort), name)})
			}
		}
		if (r.State == "stopped" || r.State == "halted") && time.Since(fi.ModTime()) < 24*time.Hour {
			a.setSub(name, func(s *sub) {
				*s = sub{Name: name, Model: r.Model, Effort: r.Effort, Window: r.Window, State: r.State, Started: fi.ModTime().UnixMilli()}
			})
		}
	}
	return out, cut
}

// resumeCut sets the subagents recoverAgents listed to work again, each
// from its saved session, and says so in the chat, starting no turn.
func (a *Agent) resumeCut(names []string) {
	var back []string
	for _, name := range names {
		var user bool
		a.note(name, func(r *agentRecord) { r.Cut, r.Resumes, user = false, r.Resumes+1, r.User })
		note := restartNote
		if user {
			note = restartUserNote
		}
		a.mu.Lock()
		out, ok := a.resume(name, []tell{{text: note}}, true, "")
		a.mu.Unlock()
		switch {
		case user: // the user's turn: Opt never hears of it
			if !ok {
				a.chat.hub.notice("warn", "[%s] %s", name, out)
				a.chat.hub.sound(soundError)
			}
		case ok:
			back = append(back, "["+name+"]")
		default:
			a.chat.Log("work", "[opt-chat] "+out)
		}
	}
	if len(back) > 0 {
		a.chat.Log("work", "[opt-chat] Started again, and resumed from their saved sessions the subagents cut off mid-task: "+strings.Join(back, ", ")+". Each reports when done.")
	}
}

// stopOrphan ends the process group of pid, a subagent's Claude Code
// that outlived a crash, its turn still going on, if it is still that
// session's: else the pid was reused.
func stopOrphan(pid int, session string) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(string(out), session) {
		return
	}
	syscall.Kill(-pid, syscall.SIGTERM)
	for range 50 { // as a call's WaitDelay; then for good
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	syscall.Kill(-pid, syscall.SIGKILL)
}

func resumeHint(name string) string {
	return fmt.Sprintf("tell(%q, message) resumes it where it stopped, with all it knew.", name)
}
