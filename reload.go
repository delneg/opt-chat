package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A reload (/reload, or SIGUSR1) builds opt-chat from the source next to
// the running binary and puts the build in its place; once the chat is
// idle, it stops every call and execs the build in place: the same pid,
// terminal, flags and web port, and the page reconnects. The subagents it
// cut off go on as it starts, from their saved sessions (see
// recoverAgents). A build that fails changes nothing, and an exec that
// fails leaves this opt-chat at work. Stage 2, which would keep the calls
// running across the exec: docs/2026-10-15-hot-reload-stage2.md.

// reloader is how a reload builds and execs: goBuild and syscall.Exec, but
// in tests.
type reloader struct {
	exe   string   // the running binary, as it was at start
	args  []string // the exec's argv: see reloadArgs
	build func(out string) error
	exec  func(exe string, argv, env []string) error
}

func newReloader(fs *flag.FlagSet) (*reloader, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return nil, err
	}
	args := append([]string{os.Args[0]}, reloadArgs(fs)...)
	return &reloader{exe: exe, args: args, build: func(out string) error { return goBuild(exe, out, args) }, exec: syscall.Exec}, nil
}

// reloadArgs are the flags this run was given, as -name=value, less an
// import's, which is done, and with -open=false: the page reconnects.
func reloadArgs(fs *flag.FlagSet) []string {
	var args []string
	fs.Visit(func(f *flag.Flag) {
		if !strings.HasPrefix(f.Name, "import") && f.Name != "open" {
			args = append(args, "-"+f.Name+"="+f.Value.String())
		}
	})
	return append(args, "-open=false")
}

// probeEnv, set, makes opt-chat stop once its flags are read and checked.
const probeEnv = "OPT_CHAT_PROBE"

// goBuild builds the source in exe's folder into out, and checks that out
// starts with argv's flags.
func goBuild(exe, out string, argv []string) error {
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("no go.mod next to %s: a reload builds opt-chat from the folder of the binary that runs", exe)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, ".")
	cmd.Dir = dir
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %v\n%s", err, strings.TrimSpace(string(b)))
	}
	probe := exec.CommandContext(ctx, out, argv[1:]...)
	probe.Env = append(os.Environ(), probeEnv+"=1")
	if b, err := probe.CombinedOutput(); err != nil {
		return fmt.Errorf("the new build won't start with this run's flags: %v\n%s", err, strings.TrimSpace(string(b)))
	}
	return nil
}

// reload builds opt-chat, puts the build in place, waits until no turn is
// underway or waiting, stops every call, and execs the build. An error:
// this opt-chat goes on.
func (h *harness) reload(r *reloader) error {
	if !h.reloading.TryLock() {
		return errors.New("a reload is underway")
	}
	defer h.reloading.Unlock()
	a := h.agent
	c := a.chat
	c.hub.notice("info", "[reload: building opt-chat]")
	c.hub.sound(soundAck) // and the new opt-chat's start
	out := r.exe + ".new"
	if err := r.build(out); err != nil {
		os.Remove(out)
		return err
	}
	if err := os.Rename(out, r.exe); err != nil { // atomic: never a half-written binary
		os.Remove(out)
		return err
	}
	c.hub.notice("info", "[reload: built; it restarts once the chat is idle]")
	if !h.hold() {
		return errors.New("opt-chat is quitting")
	}
	c.mu.Lock()
	compact := c.compact
	c.compact = nil // none starts from here on; the next run builds what is left
	c.mu.Unlock()
	a.mems.setCompact(nil)
	a.cut()
	if a.life.Err() != nil {
		return errors.New("opt-chat is quitting")
	}
	c.hub.notice("info", "[reload: restarting]")
	err := r.exec(r.exe, r.args, os.Environ())
	if err == nil { // only in tests: a real exec doesn't return
		return nil
	}
	// Still here: back to work, as at a start.
	a.mu.Lock()
	a.cutting = false
	a.mu.Unlock()
	c.mu.Lock()
	c.compact = compact
	c.pump()
	c.mu.Unlock()
	a.mems.setCompact(compact)
	h.mu.Lock()
	h.quitting = false
	h.mu.Unlock()
	msgs, cut := a.recoverAgents()
	logAll(c, msgs)
	a.resumeCut(cut)
	return fmt.Errorf("exec %s: %w", r.exe, err)
}

// hold waits until no turn is underway or waiting, then holds new ones
// off, as a quit does: what comes is logged, unanswered. False when
// quitting meanwhile.
func (h *harness) hold() bool {
	for {
		h.mu.Lock()
		quitting, running := h.quitting, h.running
		if !quitting && !running {
			h.quitting = true
		}
		h.mu.Unlock()
		if quitting || !running {
			return !quitting
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// cut stops every call, as a quit does, while opt-chat goes on: the
// subagents' records say they were cut off, for the next start to resume
// them. Until a.cutting is unset, no call starts.
func (a *Agent) cut() {
	a.mu.Lock()
	a.cutting = true
	for name, h := range a.halts {
		if r := a.resting[name]; r != nil && !r.compacting {
			r.p.close() // with its input closed, Claude Code exits at once
		} else {
			h.cancel()
		}
	}
	a.mu.Unlock()
	stopChildren(a) // compactions, side questions, Claude Code's lists
	a.calls.Wait()
}
