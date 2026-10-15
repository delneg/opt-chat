package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Wake-ups (-wake): messages Opt sets for itself, each at a time. When one
// is due, opt-chat hands it to the chat as a report, and it starts a turn.
// They are kept in <dir>/wake.jsonl, so they outlast a restart; one missed
// while opt-chat was down comes once, as it starts.

const wakeFrame = "[opt-chat] wake-up you set at "

// wakePrompt is the wake tool's line in the prompt's list, there only with -wake.
const wakePrompt = `- wake(at, message) sets a message to yourself, which starts a turn at its time
  as "` + wakeFrame + `<time>: <message>"; wake(cancel) cancels one; wake()
  lists them. A turn a wake-up started sets none.
`

type wakeUp struct {
	ID      int       `json:"id"`
	At      time.Time `json:"at"`
	Set     time.Time `json:"set"`
	Message string    `json:"message"`
}

type waker struct {
	path  string
	fire  func(text string) // hands a due wake-up to the chat
	mu    sync.Mutex
	list  []wakeUp // pending, soonest first
	timer *time.Timer
	done  bool
}

// startWaker reads the pending wake-ups in dir and arms the first.
func startWaker(dir string, fire func(text string)) (*waker, error) {
	w := &waker{path: filepath.Join(dir, "wake.jsonl"), fire: fire}
	b, err := os.ReadFile(w.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for line := range bytes.Lines(b) {
		var u wakeUp
		if json.Unmarshal(bytes.TrimSpace(line), &u) == nil {
			w.list = append(w.list, u)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sortAndArm()
	return w, nil
}

// sortAndArm sets the timer for the soonest. Under w.mu.
func (w *waker) sortAndArm() {
	slices.SortStableFunc(w.list, func(a, b wakeUp) int { return a.At.Compare(b.At) })
	if w.timer != nil {
		w.timer.Stop()
	}
	if len(w.list) > 0 && !w.done {
		w.timer = time.AfterFunc(max(0, time.Until(w.list[0].At)), w.due)
	}
}

// save writes the pending wake-ups whole. Under w.mu.
func (w *waker) save() error {
	var b []byte
	for _, u := range w.list {
		line, _ := json.Marshal(u)
		b = append(append(b, line...), '\n')
	}
	return writeAtomic(w.path, b)
}

// due takes the wake-ups whose time has come out of the file, then hands
// them on: a crash between the two loses one rather than repeats it.
func (w *waker) due() {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return
	}
	now := time.Now()
	n := 0
	for n < len(w.list) && !w.list[n].At.After(now) {
		n++
	}
	ready := slices.Clone(w.list[:n])
	w.list = w.list[n:]
	if err := w.save(); err != nil {
		fmt.Fprintf(os.Stderr, "wake.jsonl: %v\n", err)
	}
	w.sortAndArm()
	w.mu.Unlock()
	for _, u := range ready {
		w.fire(wakeFrame + u.Set.Local().Format("2006-01-02 15:04") + ": " + u.Message)
	}
}

func (w *waker) add(at time.Time, message string) (wakeUp, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	u := wakeUp{ID: 1, At: at, Set: time.Now().Truncate(time.Second), Message: message}
	for _, p := range w.list {
		u.ID = max(u.ID, p.ID+1)
	}
	w.list = append(w.list, u)
	if err := w.save(); err != nil {
		w.list = w.list[:len(w.list)-1]
		return wakeUp{}, err
	}
	w.sortAndArm()
	return u, nil
}

// cancel drops a pending wake-up; false when none has that id.
func (w *waker) cancel(id int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := slices.IndexFunc(w.list, func(u wakeUp) bool { return u.ID == id })
	if i < 0 {
		return false
	}
	w.list = slices.Delete(w.list, i, i+1)
	w.save()
	w.sortAndArm()
	return true
}

func (w *waker) pending() []wakeUp {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.list)
}

// stop fires nothing more; the file keeps what is pending.
func (w *waker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	if w.timer != nil {
		w.timer.Stop()
	}
}

// parseWhen reads a wake-up's time, local: a duration from now (90m, 2h),
// a time of day (21:30: today, or tomorrow once passed), a date and time
// (2026-10-20 09:00) or RFC 3339.
func parseWhen(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(d), nil
	}
	if t, err := time.ParseInLocation("15:04", s, time.Local); err == nil {
		at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			if !t.After(now) {
				return time.Time{}, fmt.Errorf("%s has passed", s)
			}
			return t, nil
		}
	}
	return time.Time{}, errors.New("at must be a duration (90m, 2h), a time (21:30) or a date and time (2026-10-20 09:00)")
}

func (a *Agent) setWoken(on bool) {
	a.mu.Lock()
	a.woken = on
	a.mu.Unlock()
}

// wakeOnly reports whether a turn's messages are wake-ups and reports, with
// at least one wake-up: such a turn sets no new ones.
func wakeOnly(msgs []message) bool {
	woke := false
	for _, m := range msgs {
		if m.kind == "user" {
			return false
		}
		woke = woke || m.kind == "work" && strings.HasPrefix(m.text, wakeFrame)
	}
	return woke
}

// wakeTool is the wake tool: set with at and message, cancel with cancel,
// else list.
func (a *Agent) wakeTool(fromAgent bool, x toolArgs) string {
	a.mu.Lock()
	w, woken := a.wakes, a.woken
	a.mu.Unlock()
	switch {
	case w == nil:
		return "Wake-ups are off: opt-chat runs without -wake."
	case fromAgent:
		return "Only Opt sets wake-ups."
	case x.Cancel != 0:
		if !w.cancel(x.Cancel) {
			return fmt.Sprintf("No wake-up %d pending.", x.Cancel)
		}
		return fmt.Sprintf("Wake-up %d canceled.", x.Cancel)
	case x.At == "" && strings.TrimSpace(x.Message) == "":
		list := w.pending()
		if len(list) == 0 {
			return "No wake-ups pending."
		}
		var b strings.Builder
		for _, u := range list {
			fmt.Fprintf(&b, "%d %s (set %s): %s\n", u.ID, u.At.Local().Format("2006-01-02 15:04"), u.Set.Local().Format("2006-01-02 15:04"), u.Message)
		}
		return strings.TrimSuffix(b.String(), "\n")
	case woken:
		return "A turn a wake-up started sets no new wake-ups."
	case strings.TrimSpace(x.Message) == "":
		return "No message given."
	}
	at, err := parseWhen(x.At, time.Now())
	if err != nil {
		return err.Error() + "."
	}
	u, err := w.add(at, strings.TrimSpace(x.Message))
	if err != nil {
		return "Not set: " + err.Error() + "."
	}
	return fmt.Sprintf("Wake-up %d set for %s.", u.ID, u.At.Local().Format("2006-01-02 15:04"))
}
