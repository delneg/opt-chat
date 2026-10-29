package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A loop keeps a subagent at work turn after turn, as a Stop hook keeps
// Claude Code at work. Hooks stay off in every call (see ownSettings), so
// opt-chat runs the hook itself at the end of each turn: a "block" sends
// its reason back as the next message, and the chat sees nothing until the
// loop ends. Opt's tells wait for the turn's end and go first.
type loopHook struct {
	Command string `json:"command"`      // run by sh in Dir, Claude Code's Stop hook input on stdin
	Dir     string `json:"dir"`          // the input's cwd
	Max     int    `json:"max,omitzero"` // turns it starts before a report; 0 is loopMax

	timeout time.Duration // loopTimeout, when set (tests)
}

const loopMax = 60

const loopTimeout = 60 * time.Second // as Claude Code's default for a hook

const (
	loopFeedback = "Stop hook feedback:\n" // as Claude Code passes a block's reason
)

// check says what is wrong with l, or "".
func (l *loopHook) check() string {
	if strings.TrimSpace(l.Command) == "" {
		return "loop needs a command."
	}
	if fi, err := os.Stat(l.Dir); !filepath.IsAbs(l.Dir) || err != nil || !fi.IsDir() {
		return "loop's dir must be an absolute path to a folder."
	}
	if l.Max < 0 {
		return "loop's max must be 0 (60 turns) or more."
	}
	return ""
}

func (l *loopHook) max() int {
	if l.Max == 0 {
		return loopMax
	}
	return l.Max
}

// stop runs the hook at the end of a turn whose last text was last: the
// reason to go on, or "" to stop. again: that turn was the hook's.
func (l *loopHook) stop(ctx context.Context, session, last string, again bool) (string, error) {
	in, _ := json.Marshal(map[string]any{"session_id": session, "transcript_path": sessionPath(session), "cwd": l.Dir,
		"hook_event_name": "Stop", "stop_hook_active": again, "last_assistant_message": last})
	limit := cmp.Or(l.timeout, loopTimeout)
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var out, errs bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", l.Command)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = l.Dir, append(childEnv(), "CLAUDE_PROJECT_DIR="+l.Dir), bytes.NewReader(in), &out, &errs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // its children die with it
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("timed out after %v", limit)
	case errors.As(err, &exit) && exit.ExitCode() == 2: // as Claude Code: blocks, stderr its reason
		return strings.TrimSpace(errs.String()), nil
	case err != nil:
		return "", fmt.Errorf("%v: %s", err, clip(strings.TrimSpace(errs.String()), 500))
	}
	var r struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Continue *bool  `json:"continue"`
	}
	if json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r) != nil || r.Decision != "block" || r.Continue != nil && !*r.Continue {
		return "", nil
	}
	return strings.TrimSpace(r.Reason), nil
}

// loopState is a subagent's loop, in its call.
type loopState struct {
	hook       *loopHook
	turns      int    // started by the hook since the last report
	again      bool   // the turn ending now was the hook's
	compacting bool   // between turns: next goes after
	next       []tell // the next message's
}

// looping says whether name is in a loop; a.mu held.
func (a *Agent) looping(name string) bool {
	_, ok := a.held[name]
	return ok
}

// takeHeld gives the tells held for name, logged now, as it gets them.
func (a *Agent) takeHeld(name string) []tell {
	a.mu.Lock()
	q := a.held[name]
	if len(q) > 0 {
		a.held[name] = nil
	}
	a.mu.Unlock()
	a.logTells(name, q)
	return q
}

// loopStep, as a turn ends, gives the next message's tells, or none for a
// report, with what to add to it: Opt's and the user's first, then the hook's.
func (a *Agent) loopStep(name string, h *halt, lp *loopState, session string, e event) ([]tell, string) {
	if q := a.takeHeld(name); len(q) > 0 {
		lp.again = false
		return q, ""
	}
	if e.IsError {
		lp.turns, lp.again = 0, false
		return nil, ""
	}
	var why string
	if lp.turns >= lp.hook.max() {
		why = fmt.Sprintf("%d turns, its cap", lp.turns)
	} else {
		reason, err := lp.hook.stop(h.ctx, session, e.Result, lp.again)
		switch {
		case h.ctx.Err() != nil:
			return nil, ""
		case err != nil:
			why = "its Stop hook failed: " + err.Error()
		case reason != "":
			if q := a.takeHeld(name); len(q) > 0 { // came while the hook ran
				lp.again = false
				return q, ""
			}
			lp.turns++
			lp.again = true
			a.agentWrite(name, "loop", reason)
			return []tell{{text: loopFeedback + reason}}, ""
		case lp.turns == 0: // the hook never held it: this was its report
			return nil, ""
		default:
			why = "its Stop hook let it stop"
		}
	}
	lp.turns, lp.again = 0, false
	return nil, "\n\n(opt-chat: the loop stopped: " + why + ".)"
}
