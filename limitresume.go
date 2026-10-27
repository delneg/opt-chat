package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
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
// session. Each waits for its own reset, since a model's weekly limit stops
// that model's calls alone. A limit that hits again moves the time; a stale
// time or a string of hits backs off, so nothing loops faster than the
// reset. One that runs again and finishes waits no more.

// limitMax is how many resumes in a row a limit gets before opt-chat stops
// resuming on its own; one that went well starts the count over.
const limitMax = 6

// limitNote is what a stopped subagent is told as it resumes.
const limitNote = "The usage limit has reset. Pick up where you stopped: check what state your files, workspace and branch are in before you redo anything, then finish the task as given."

var (
	// limitText is the plan's usage limit as Claude Code words it: "You've
	// hit your session limit", "your weekly usage limit", "Usage limit
	// reached", "5-hour limit reached", "Your limit will reset", or a spend
	// limit hit because a plan window ran out, which says "your session limit
	// resets 3:45pm". It names a plan window or none, so the context, rate,
	// max turns and output token limits, spend limits, usage credits and
	// monthly limits don't match. It backs up the rate_limit_event, which
	// says the same without words: see refusedLocked.
	limitText = regexp.MustCompile(`(?i)\b(?:hit|reached) your (?:(?:session|weekly|5-hour|opus|sonnet|fable) )?(?:usage )?limit\b|(?:^|[^\w\s'’]\s*|\bclaude (?:ai )?)usage limit reached|\b(?:session|weekly|5-hour|opus|sonnet|fable)(?: weekly)? limit reached|\byour limit will reset|\byour (?:session|weekly|5-hour|opus|sonnet|fable) limit resets\b`)
	epochRe   = regexp.MustCompile(`\|(\d{10})(?:\d{3})?\b`)
	inRe      = regexp.MustCompile(`(?i)\bresets?\s+in\s+(\d+)\s*(hours?|hrs?|h|minutes?|mins?|m)\b`)
	atRe      = regexp.MustCompile(`(?i)\bresets?(?:\s+at)?\s+(?:(mon|tue|wed|thu|fri|sat|sun)[a-z]*\.?,?\s+)?(?:([a-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s+(?:at\s+)?)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)?(?:\s*\(([\w/+\-]+)\))?`)
)

// usageLimit tells whether a failure is the plan's usage limit.
func usageLimit(text string) bool { return limitText.MatchString(text) }

// justPassed is how long after a reset time named without a date its
// message can be read and still mean that time, not the next day's or week's.
const justPassed = 15 * time.Minute

// limitReset reads when the limit resets from Claude Code's message, as
// "You've hit your session limit · resets 4am (America/New_York)",
// "resets Mon 12:00am", "resets Oct 24, 10am (UTC)", "resets 16:00", "resets in 2 hours" or the
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
	hour, _ := strconv.Atoi(m[4])
	min, _ := strconv.Atoi(m[5])
	switch strings.ToLower(m[6]) {
	case "":
		if m[5] == "" { // a bare number is no time
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
	if l, err := time.LoadLocation(m[7]); m[7] != "" && err == nil {
		loc = l
	}
	n := now.In(loc)
	if m[2] == "" { // today or the next day at that time, or the weekday named
		at := time.Date(n.Year(), n.Month(), n.Day(), hour, min, 0, 0, loc)
		step := 1
		if m[1] != "" {
			wd := strings.Index("sunmontuewedthufrisat", strings.ToLower(m[1][:3])) / 3
			at = at.AddDate(0, 0, (wd-int(n.Weekday())+7)%7)
			step = 7
		}
		if at.Before(now.Add(-justPassed)) {
			at = at.AddDate(0, 0, step)
		}
		return at, true
	}
	mon, err := time.Parse("Jan", m[2][:3])
	day, _ := strconv.Atoi(m[3])
	if err != nil || day < 1 || day > 31 {
		return time.Time{}, false
	}
	at := time.Date(n.Year(), mon.Month(), day, hour, min, 0, 0, loc)
	if at.Before(now.Add(-24 * time.Hour)) {
		at = at.AddDate(1, 0, 0)
	}
	return at, true
}

// resumeState is what resume.json keeps: who the limit stopped, each with
// when it resumes, and how many resumes have gone out since one last went
// well.
type resumeState struct {
	Due   map[string]time.Time `json:"due,omitzero"` // by subagent name; "" is Opt's turn
	Tries int                  `json:"tries,omitzero"`
	Fired time.Time            `json:"fired,omitzero"` // the last resume that went out
}

func (s resumeState) pending() bool { return len(s.Due) > 0 }

// next is the earliest resume pending.
func (s resumeState) next() time.Time {
	var at time.Time
	for _, t := range s.Due {
		if at.IsZero() || t.Before(at) {
			at = t
		}
	}
	return at
}

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
		r.timer = time.AfterFunc(max(0, time.Until(r.st.next())), r.due)
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
// reset: who's resume goes out grace after it, never sooner than a back-off
// that grows with each resume in a row, nor sooner than one already set for
// who. It returns when; ok false: this many in a row, so none is set.
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
	if r.st.Due == nil {
		r.st.Due = map[string]time.Time{}
	}
	if r.st.Due[who].Before(at) {
		r.st.Due[who] = at
	}
	r.save()
	r.arm()
	return r.st.Due[who], true
}

// due takes the resumes whose time has come out of the file, then hands
// them on: a crash between the two loses them rather than repeats them.
func (r *resumer) due() {
	r.mu.Lock()
	now := time.Now()
	out := resumeState{Due: map[string]time.Time{}, Tries: r.st.Tries}
	for who, at := range r.st.Due {
		if !at.After(now) {
			out.Due[who] = at
		}
	}
	if r.done || !out.pending() { // none yet: moved later while the timer ran
		r.arm()
		r.mu.Unlock()
		return
	}
	for who := range out.Due {
		delete(r.st.Due, who)
	}
	r.st.Tries, r.st.Fired = out.Tries+1, now
	r.save()
	r.arm()
	r.mu.Unlock()
	r.fire(out)
}

// went is told that who's call finished without the limit: who waits no
// more, and the count starts over. A call that failed some other way leaves
// who waiting, to go on at the reset.
func (r *resumer) went(who string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, waits := r.st.Due[who]; !waits && r.st.Tries == 0 {
		return
	}
	delete(r.st.Due, who)
	r.st.Tries = 0
	if !r.st.pending() {
		r.st = resumeState{}
	}
	r.save()
	r.arm()
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
	now := time.Now()
	refused, until := a.refusedLocked(now)
	if r == nil || !refused && !usageLimit(text) {
		return ""
	}
	reset, ok := limitReset(text, now)
	switch {
	case ok:
	case refused:
		reset = until
	default:
		reset = a.nextResetLocked(now)
	}
	at, ok := r.note(who, reset, grace, now)
	if !ok {
		return fmt.Sprintf("opt-chat resumed after this limit %d times in a row, so it does not again on its own.", limitMax)
	}
	return "opt-chat starts a turn at " + at.Local().Format("Jan 2 15:04") + ", after the reset, and resumes the subagents the limit stopped."
}

// refusedLocked, a.mu held, tells whether Claude Code's last rate_limit_event
// said the plan refuses every request, and until when: a window all models
// share, not usage credits nor one only bought credits lift, and not past its
// reset. One model's own window (seven_day_opus, seven_day_sonnet) refuses
// that model's calls alone, and the event does not say which call saw it, so
// its words are what tell. Status and resetsAt are the Agent SDK's
// documented fields; the rest only narrow.
func (a *Agent) refusedLocked(now time.Time) (bool, time.Time) {
	l := a.refused
	paid := l.OverageStatus == "allowed" || l.OverageStatus == "allowed_warning"
	shared := l.RateLimitType == "" || l.RateLimitType == "five_hour" || l.RateLimitType == "seven_day"
	if l.Status != "rejected" || paid || !shared || l.ErrorCode != "" || l.ResetsAt <= now.Unix() {
		return false, time.Time{}
	}
	return true, time.Unix(l.ResetsAt, 0)
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

// limitWent tells the resumer that who ("" for Opt's turn) finished
// without the limit.
func (a *Agent) limitWent(who string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.limitWentLocked(who)
}

// limitWentLocked is limitWent with a.mu held.
func (a *Agent) limitWentLocked(who string) {
	if a.resumes != nil {
		a.resumes.went(who)
	}
}

// resumeAfterLimit is the resume at the reset: each subagent the limit
// stopped goes on from its saved session, and one turn starts that says
// what stopped.
func (a *Agent) resumeAfterLimit(s resumeState) {
	var stopped, back, left []string
	if _, ok := s.Due[""]; ok {
		stopped = append(stopped, "Opt's turn")
	}
	var last time.Time
	for _, at := range s.Due {
		if at.After(last) {
			last = at
		}
	}
	for _, name := range slices.Sorted(maps.Keys(s.Due)) {
		if name == "" {
			continue
		}
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
	text := "[opt-chat] usage limit reset at " + last.Local().Format("Jan 2 15:04") + "; stopped by it: " + strings.Join(stopped, ", ") + "."
	if len(back) > 0 {
		text += " Resumed from their saved sessions: " + strings.Join(back, ", ") + ". Each reports when done."
	}
	if len(left) > 0 {
		text += " Not resumed: " + strings.Join(left, " ")
	}
	a.report(text)
}
