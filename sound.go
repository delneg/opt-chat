package main

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Sounds: opt-chat plays what happens, as peon-ping plays it for Claude
// Code (github.com/PeonPing/peon-ping), whose hooks never run in opt-chat's
// calls (see ownSettings): Opt takes the user's message and ends its turn,
// a subagent starts, reports or fails, a question waits, something breaks.
// The sounds come from the pack peon-ping plays, in the format of CESP, its
// sound pack spec: a pack's openpeon.json lists its sounds by category.
// peon-ping's config.json says which pack, how loud, and whether sounds are
// on (peon toggle); it is read at each sound, so a change there is heard at
// once. Its per-category switches are Claude Code's events, not these, and
// are not read.

// The categories of CESP that opt-chat plays, the most important first: of
// sounds that come together, only that one plays.
const (
	soundError = "task.error"       // a failure: a turn, a subagent, a reload
	soundAsk   = "input.required"   // a tool call waits for the user's yes
	soundLimit = "resource.limit"   // a subagent's context compacted
	soundDone  = "task.complete"    // Opt ended its turn, idle; a subagent reported
	soundStart = "session.start"    // opt-chat started; a subagent started
	soundAck   = "task.acknowledge" // Opt or a subagent took the user's message
)

var soundOrder = []string{soundError, soundAsk, soundLimit, soundDone, soundStart, soundAck}

// soundsTogether is how long a sound waits for others that come with it,
// as when several subagents start at once: the most important of them
// plays, once.
const soundsTogether = 300 * time.Millisecond

type sounder struct {
	dir  string                                  // peon-ping's: config.json, packs/
	play func(file string, volume float64) error // the sound, to its end
	in   chan string
	last map[string]string // the file each category played last: not twice in a row; run's, one sound at a time
	// chimes are the categories played by a sound of the system's in place
	// of the pack's, when the file is there.
	chimes map[string]string
}

// peonDir is where peon-ping keeps its config and packs.
func peonDir() string {
	if d := os.Getenv("CLAUDE_PEON_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "hooks", "peon-ping")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "hooks", "peon-ping")
}

// newSounder plays the sounds of peon-ping's pack in dir; nil when peon-ping
// is not there.
func newSounder(dir string) *sounder {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return nil
	}
	s := &sounder{dir: dir, play: playFile, in: make(chan string, 64), last: map[string]string{}, chimes: systemChimes()}
	go s.run()
	return s
}

// systemChimes are the categories a Mac plays by one of its own sounds: a
// compaction is routine, and a pack's resource.limit is often an alarm.
func systemChimes() map[string]string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return map[string]string{soundLimit: "/System/Library/Sounds/Purr.aiff"}
}

// sound plays a sound of category cat, unless one more important comes with
// it; never waits.
func (s *sounder) sound(cat string) {
	if s == nil {
		return
	}
	select {
	case s.in <- cat:
	default: // a storm of them: this one goes unheard
	}
}

// run plays the sounds one at a time. What comes while one plays waits for
// its end, the most important of it alone.
func (s *sounder) run() {
	next := ""
	more := func(cat string) {
		if next == "" || slices.Index(soundOrder, cat) < slices.Index(soundOrder, next) {
			next = cat
		}
	}
	for {
		if next == "" {
			next = <-s.in
		}
		together := time.After(soundsTogether)
	gather:
		for {
			select {
			case cat := <-s.in:
				more(cat)
			case <-together:
				break gather
			}
		}
		cat := next
		next = ""
		done := make(chan struct{})
		go func() {
			if err := s.playOne(cat); err != nil {
				dim("[sound: " + err.Error() + "]")
			}
			close(done)
		}()
		for playing := true; playing; {
			select {
			case c := <-s.in:
				more(c)
			case <-done:
				playing = false
			}
		}
	}
}

// peonConfig is what opt-chat reads of peon-ping's config.json.
type peonConfig struct {
	Enabled     *bool    `json:"enabled"`
	Volume      *float64 `json:"volume"`
	DefaultPack string   `json:"default_pack"`
	ActivePack  string   `json:"active_pack"` // its name before default_pack
}

// cespPack is what opt-chat reads of a pack's manifest.
type cespPack struct {
	Categories map[string]struct {
		Sounds []struct {
			File string `json:"file"`
		} `json:"sounds"`
	} `json:"categories"`
}

// playOne plays a sound of category cat from the pack peon-ping plays now,
// or its chime, unless its sounds are off or the pack has none of cat.
func (s *sounder) playOne(cat string) error {
	b, err := os.ReadFile(filepath.Join(s.dir, "config.json"))
	if err != nil {
		return err
	}
	var cfg peonConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil
	}
	volume := 0.5
	if cfg.Volume != nil {
		volume = min(max(*cfg.Volume, 0), 1)
	}
	if f := s.chimes[cat]; f != "" {
		if _, err := os.Stat(f); err == nil {
			return s.play(f, volume)
		}
	}
	name := cmp.Or(cfg.DefaultPack, cfg.ActivePack, "peon")
	if name != filepath.Base(name) {
		return errors.New("pack name " + strconv.Quote(name) + " is not a folder's")
	}
	pack := filepath.Join(s.dir, "packs", name)
	if b, err = os.ReadFile(filepath.Join(pack, "openpeon.json")); errors.Is(err, os.ErrNotExist) {
		b, err = os.ReadFile(filepath.Join(pack, "manifest.json")) // an older pack's
	}
	if err != nil {
		return err
	}
	var m cespPack
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	var files []string
	for _, f := range m.Categories[cat].Sounds {
		if f.File != "" && filepath.IsLocal(f.File) {
			files = append(files, f.File)
		}
	}
	if len(files) == 0 {
		return nil
	}
	if len(files) > 1 {
		files = slices.DeleteFunc(files, func(f string) bool { return f == s.last[cat] })
	}
	file := files[rand.IntN(len(files))]
	s.last[cat] = file
	if !strings.Contains(file, "/") { // an older pack's: bare names, in sounds/
		file = filepath.Join("sounds", file)
	}
	return s.play(filepath.Join(pack, file), volume)
}

// playFile plays the sound file at path, at volume from 0 to 1, as
// peon-ping does: afplay on a Mac, else the first player found.
func playFile(path string, volume float64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second) // a player stuck never blocks the next sound for long
	defer cancel()
	var cmd *exec.Cmd
	has := func(p string) bool { _, err := exec.LookPath(p); return err == nil }
	v := strconv.FormatFloat(volume, 'f', 2, 64)
	switch {
	case runtime.GOOS == "darwin":
		cmd = exec.CommandContext(ctx, "afplay", "-v", v, path)
	case has("pw-play"):
		cmd = exec.CommandContext(ctx, "pw-play", "--volume", v, path)
	case has("paplay"):
		cmd = exec.CommandContext(ctx, "paplay", "--volume="+strconv.Itoa(int(volume*65536)), path)
	case has("ffplay"):
		cmd = exec.CommandContext(ctx, "ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet", "-volume", strconv.Itoa(int(volume*100)), path)
	case has("aplay"):
		cmd = exec.CommandContext(ctx, "aplay", "-q", path)
	default:
		return errors.New("no player for sounds: afplay, pw-play, paplay, ffplay or aplay")
	}
	return cmd.Run()
}
