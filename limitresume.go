package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Resume at the reset (-resume-at-reset): when the plan's usage limit stops
// Opt's turn or a subagent, the call ends with a message that says when the
// limit resets. opt-chat reads that time, keeps it in <dir>/resume.json (so
// it outlasts a restart, as wake-ups do), and at the reset starts one turn
// and sets each subagent the limit stopped to work again from its saved
// session. A limit that hits again moves the time; a stale time or a
// string of hits backs off, so nothing loops faster than the reset.

// limitMax is how many resumes in a row a limit gets before opt-chat stops
// resuming on its own; one that went well starts the count over.
const limitMax = 6

// limitNote is what a stopped subagent is told as it resumes.
const limitNote = "The usage limit has reset. Pick up where you stopped: check what state your files, workspace and branch are in before you redo anything, then finish the task as given."

var (
	limitText = regexp.MustCompile(`(?i)limit reached|(?:hit|reached) your [\w ]*limit|limit will reset`)
	epochRe   = regexp.MustCompile(`\|(\d{10})(?:\d{3})?\b`)
	inRe      = regexp.MustCompile(`(?i)\bresets?\s+in\s+(\d+)\s*(hours?|hrs?|h|minutes?|mins?|m)\b`)
	atRe      = regexp.MustCompile(`(?i)\bresets?(?:\s+at)?\s+(?:([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)?(?:\s*\(([\w/+\-]+)\))?`)
)

// usageLimit tells whether a failure is the plan's usage limit.
func usageLimit(text string) bool { return limitText.MatchString(text) }

// limitReset reads when the limit resets from Claude Code's message, as
// "You've hit your session limit · resets 4am (America/New_York)",
// "resets Oct 24, 10am (UTC)", "resets 16:00", "resets in 2 hours" or the
// older "usage limit reached|1760000000".
func limitReset(text string, now time.Time) (time.Time, bool) {
	if m := epochRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		return time.Unix(n, 0), true
	}
	if m := inRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		d := time.Duration(n) * time.Minute
		if strings.HasPrefix(strings.ToLower(m[2]), "h") {
			d = time.Duration(n) * time.Hour
		}
		return now.Add(d), true
	}
	m := atRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[3])
	min, _ := strconv.Atoi(m[4])
	switch strings.ToLower(m[5]) {
	case "":
		if m[4] == "" { // a bare number is no time
			return time.Time{}, false
		}
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || min > 59 {
		return time.Time{}, false
	}
	loc := time.Local
	if l, err := time.LoadLocation(m[6]); m[6] != "" && err == nil {
		loc = l
	}
	n := now.In(loc)
	if m[1] == "" {
		at := time.Date(n.Year(), n.Month(), n.Day(), hour, min, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, true
	}
	mon, err := time.Parse("Jan", m[1][:3])
	day, _ := strconv.Atoi(m[2])
	if err != nil || day < 1 || day > 31 {
		return time.Time{}, false
	}
	at := time.Date(n.Year(), mon.Month(), day, hour, min, 0, 0, loc)
	if at.Before(now.Add(-24 * time.Hour)) {
		at = at.AddDate(1, 0, 0)
	}
	return at, true
}

// resumeState is what resume.json keeps: the resume pending, if any, and
// how many have gone out since one last went well.
type resumeState struct {
	At     time.Time `json:"at,omitzero"`
	Turn   bool      `json:"turn,omitzero"`   // Opt's own turn was stopped
	Agents []string  `json:"agents,omitzero"` // subagents stopped
	Tries  int       `json:"tries,omitzero"`
	Fired  time.Time `json:"fired,omitzero"` // the last resume that went out
}

func (s resumeState) pending() bool { return s.Turn || len(s.Agents) > 0 }

type resumer struct {
	path  string
	fire  func(resumeState)
	mu    sync.Mutex
	st    resumeState
	timer *time.Timer
	done  bool
}

// startResumer reads resume.json in dir and arms the pending resume; one
// missed while opt-chat was down goes out as it starts.
func startResumer(dir string, fire func(resumeState)) (*resumer, error) {
	r := &resumer{path: filepath.Join(dir, "resume.json"), fire: fire}
	b, err := os.ReadFile(r.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if len(b) > 0 && json.Unmarshal(b, &r.st) != nil {
		r.st = resumeState{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.arm()
	return r, nil
}

// arm sets the timer for the pending resume. Under r.mu.
func (r *resumer) arm() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.st.pending() && !r.done {
		r.timer = time.AfterFunc(max(0, time.Until(r.st.At)), r.due)
	}
}

func (r *resumer) save() {
	b, err := json.Marshal(r.st)
	if err == nil {
		err = writeAtomic(r.path, b)
	}
	if err != nil {
		warn("resume.json: %v", err)
	}
}

// note records that the limit stopped who ("" for Opt's turn) and resets at
// reset: the resume goes out grace after it, and never sooner than a back-off
// that grows with each resume in a row. It returns when; ok false: this many
// in a row, so none is set.
func (r *resumer) note(who string, reset time.Time, grace time.Duration, now time.Time) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tries := r.st.Tries
	if !r.st.pending() && now.Sub(r.st.Fired) > 6*time.Hour {
		tries = 0
	}
	if tries >= limitMax {
		return time.Time{}, false
	}
	at := reset.Add(grace)
	if floor := now.Add(time.Duration(tries+1) * 5 * time.Minute); !reset.After(now) && at.Before(floor) {
		at = floor // a reset already past: the message was stale, or the limit is not over
	}
	r.st.Tries = tries
	if r.st.At.Before(at) {
		r.st.At = at
	}
	if who == "" {
		r.st.Turn = true
	} else if !slices.Contains(r.st.Agents, who) {
		r.st.Agents = append(r.st.Agents, who)
	}
	r.save()
	r.arm()
	return r.st.At, true
}

// due takes the pending resume out of the file, then hands it on: a crash
// between the two loses one rather than repeats it.
func (r *resumer) due() {
	r.mu.Lock()
	if r.done || !r.st.pending() {
		r.mu.Unlock()
		return
	}
	if d := time.Until(r.st.At); d > 0 { // moved later while the timer ran
		r.arm()
		r.mu.Unlock()
		return
	}
	out := r.st
	r.st = resumeState{Tries: out.Tries + 1, Fired: time.Now()}
	r.save()
	r.mu.Unlock()
	r.fire(out)
}

// went clears the count: a turn or a subagent finished without the limit.
func (r *resumer) went() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.st.Tries > 0 && !r.st.pending() {
		r.st = resumeState{}
		r.save()
	}
}

func (r *resumer) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	if r.timer != nil {
		r.timer.Stop()
	}
}

// limitHit, given a failed call's message, notes the resume when it is the
// usage limit: who is the subagent, or "" for Opt's turn. It returns the
// line for the chat that says so, or "".
func (a *Agent) limitHit(text, who string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.limitHitLocked(text, who)
}

// limitHitLocked is limitHit with a.mu held.
func (a *Agent) limitHitLocked(text, who string) string {
	r, grace := a.resumes, a.resetGrace
	if r == nil || !usageLimit(text) {
		return ""
	}
	now := time.Now()
	reset, ok := limitReset(text, now)
	if !ok {
		reset = a.nextResetLocked(now)
	}
	at, ok := r.note(who, reset, grace, now)
	if !ok {
		return fmt.Sprintf("opt-chat resumed after this limit %d times in a row, so it does not again on its own.", limitMax)
	}
	return "opt-chat starts a turn at " + at.Local().Format("Jan 2 15:04") + ", after the reset, and resumes the subagents the limit stopped."
}

// nextResetLocked, a.mu held, is when a limit that gave no time resets: the latest of the
// windows used up, else the five-hour one, else an hour from now.
func (a *Agent) nextResetLocked(now time.Time) time.Time {
	at := now.Add(time.Hour)
	var full, five int64
	for _, w := range a.limits {
		switch {
		case w.Used >= 99.5 && w.Resets > now.Unix():
			full = max(full, w.Resets)
		case w.Name == "five_hour" && w.Resets > now.Unix():
			five = w.Resets
		}
	}
	if t := cmpFirst(full, five); t > 0 {
		at = time.Unix(t, 0)
	}
	return at
}

func cmpFirst(a, b int64) int64 {
	if a > 0 {
		return a
	}
	return b
}

// limitWent tells the resumer that something finished without the limit.
func (a *Agent) limitWent() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.limitWentLocked()
}

// limitWentLocked is limitWent with a.mu held.
func (a *Agent) limitWentLocked() {
	if a.resumes != nil {
		a.resumes.went()
	}
}

// resumeAfterLimit is the resume at the reset: each subagent the limit
// stopped goes on from its saved session, and one turn starts that says
// what stopped.
func (a *Agent) resumeAfterLimit(s resumeState) {
	var stopped, back, left []string
	if s.Turn {
		stopped = append(stopped, "Opt's turn")
	}
	for _, name := range s.Agents {
		stopped = append(stopped, "["+name+"]")
		a.mu.Lock()
		_, working := a.running[name]
		a.mu.Unlock()
		if working { // already at work again: the user told it something
			continue
		}
		if out, ok := a.tell(name, tell{text: limitNote}); ok {
			back = append(back, "["+name+"]")
		} else {
			left = append(left, out)
		}
	}
	text := "[opt-chat] usage limit reset at " + s.At.Local().Format("Jan 2 15:04") + "; stopped by it: " + strings.Join(stopped, ", ") + "."
	if len(back) > 0 {
		text += " Resumed from their saved sessions: " + strings.Join(back, ", ") + ". Each reports when done."
	}
	if len(left) > 0 {
		text += " Not resumed: " + strings.Join(left, " ")
	}
	a.report(text)
}
