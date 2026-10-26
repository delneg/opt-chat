package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// peonFixture is a peon-ping folder: config, and a pack whose manifest is
// named manifest, holding these sounds by category.
func peonFixture(t *testing.T, config, manifest string, sounds map[string][]string) string {
	t.Helper()
	dir := t.TempDir()
	pack := filepath.Join(dir, "packs", "pk")
	if err := os.MkdirAll(filepath.Join(pack, "sounds"), 0o700); err != nil {
		t.Fatal(err)
	}
	var cats []string
	for cat, files := range sounds {
		var list []string
		for _, f := range files {
			list = append(list, `{"file":"`+f+`"}`)
		}
		cats = append(cats, `"`+cat+`":{"sounds":[`+strings.Join(list, ",")+`]}`)
	}
	for name, s := range map[string]string{
		filepath.Join(dir, "config.json"): config,
		filepath.Join(pack, manifest):     `{"cesp_version":"1.0","name":"pk","categories":{` + strings.Join(cats, ",") + `}}`,
	} {
		if err := os.WriteFile(name, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type played struct {
	file   string
	volume float64
}

// testSounder plays to a channel, not a speaker, with these chimes.
func testSounder(dir string, chimes map[string]string) (*sounder, chan played) {
	ch := make(chan played, 16)
	s := &sounder{dir: dir, in: make(chan string, 64), last: map[string]string{}, chimes: chimes,
		play: func(file string, v float64) error { ch <- played{file, v}; return nil }}
	go s.run()
	return s, ch
}

func heard(t *testing.T, ch chan played) played {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no sound played")
		return played{}
	}
}

func silence(t *testing.T, ch chan played) {
	t.Helper()
	select {
	case p := <-ch:
		t.Fatalf("played %+v", p)
	case <-time.After(2 * soundsTogether):
	}
}

// Sounds come from the pack peon-ping's config names, at its volume; of
// those that come together the most important plays, once; a category's
// sound is not played twice in a row; a sound outside the pack is never
// played.
func TestSounds(t *testing.T) {
	dir := peonFixture(t, `{"enabled":true,"volume":0.3,"default_pack":"pk","categories":{"task.complete":false}}`, "openpeon.json", map[string][]string{
		soundAck:   {"sounds/ack.wav"},
		soundError: {"sounds/err.wav"},
		soundDone:  {"sounds/done1.wav", "sounds/done2.wav"},
		soundStart: {"../../escape.wav"},
	})
	s, ch := testSounder(dir, nil)
	s.sound(soundAck)
	s.sound(soundError)
	s.sound(soundAck)
	if p := heard(t, ch); p.file != filepath.Join(dir, "packs", "pk", "sounds", "err.wav") || p.volume != 0.3 {
		t.Fatalf("played %+v, want the error's alone, at 0.3", p)
	}
	silence(t, ch)
	s.sound(soundDone) // peon-ping's switch for task.complete is Claude Code's, not opt-chat's
	first := heard(t, ch).file
	s.sound(soundDone)
	if again := heard(t, ch).file; again == first {
		t.Fatalf("%s played twice in a row", first)
	}
	s.sound(soundStart)
	s.sound(soundLimit) // none in the pack
	silence(t, ch)
}

// peon toggle mutes them; an older pack's manifest.json and bare file
// names play; without peon-ping, none.
func TestSoundsOff(t *testing.T) {
	dir := peonFixture(t, `{"enabled":false,"active_pack":"pk"}`, "manifest.json", map[string][]string{soundAck: {"ack.wav"}})
	s, ch := testSounder(dir, nil)
	s.sound(soundAck)
	silence(t, ch)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"active_pack":"pk"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.sound(soundAck)
	if p := heard(t, ch); p.file != filepath.Join(dir, "packs", "pk", "sounds", "ack.wav") || p.volume != 0.5 {
		t.Fatalf("played %+v", p)
	}
	if newSounder(t.TempDir()) != nil {
		t.Fatal("sounds without peon-ping")
	}
	var nobody *hub
	nobody.sound(soundAck)
}

// A category with a chime plays it, at the pack's volume, in place of the
// pack's sound; one whose chime is missing plays the pack's.
func TestSoundChime(t *testing.T) {
	dir := peonFixture(t, `{"volume":0.3,"default_pack":"pk"}`, "openpeon.json", map[string][]string{
		soundLimit: {"sounds/limit.wav"}, soundAck: {"sounds/ack.wav"},
	})
	chime := filepath.Join(dir, "chime.aiff")
	if err := os.WriteFile(chime, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, ch := testSounder(dir, map[string]string{soundLimit: chime, soundAck: filepath.Join(dir, "gone.aiff")})
	s.sound(soundLimit)
	if p := heard(t, ch); p != (played{chime, 0.3}) {
		t.Fatalf("played %+v, want the chime", p)
	}
	s.sound(soundAck)
	if p := heard(t, ch); !strings.HasSuffix(p.file, "ack.wav") {
		t.Fatalf("played %+v, want the pack's", p)
	}
}

// The user's message is taken and Opt's turn ends: the last sound is the
// end's; a turn the user stops ends without one.
func TestTurnSounds(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	dir := peonFixture(t, `{"default_pack":"pk"}`, "openpeon.json", map[string][]string{
		soundAck: {"sounds/ack.wav"}, soundDone: {"sounds/done.wav"}, soundError: {"sounds/err.wav"},
	})
	s, ch := testSounder(dir, nil)
	c.hub.sounds = s
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "hello"})
	for p := heard(t, ch); !strings.HasSuffix(p.file, "done.wav"); p = heard(t, ch) {
		if !strings.HasSuffix(p.file, "ack.wav") {
			t.Fatalf("played %s", p.file)
		}
	}
	waitFor(t, "the turn to end", h.idle)
	h.input(message{kind: "user", text: "stream-then-stop"})
	if p := heard(t, ch); !strings.HasSuffix(p.file, "ack.wav") {
		t.Fatalf("played %s, want the message taken", p.file)
	}
	waitFor(t, "the reply to stream", func() bool {
		c.hub.mu.Lock()
		defer c.hub.mu.Unlock()
		return c.hub.turn.Text == "half a reply"
	})
	h.stop()
	waitFor(t, "the turn to end", h.idle)
	silence(t, ch)
}

// A subagent's start, its report, and its context compacted mid-task are
// heard.
func TestAgentSounds(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	dir := peonFixture(t, `{"default_pack":"pk"}`, "openpeon.json", map[string][]string{
		soundStart: {"sounds/start.wav"}, soundDone: {"sounds/done.wav"}, soundLimit: {"sounds/limit.wav"}, soundError: {"sounds/err.wav"},
	})
	s, ch := testSounder(dir, nil)
	c.hub.sounds = s
	a, _ := fakeAgent(t, c)
	a.report = func(string) {}
	until := func(want string) {
		t.Helper()
		for p := heard(t, ch); !strings.HasSuffix(p.file, want); p = heard(t, ch) {
			if strings.HasSuffix(p.file, "err.wav") {
				t.Fatalf("played %s, waiting for %s", p.file, want)
			}
		}
	}
	a.Spawn("Scout", "look around", "", "", 0, nil)
	until("done.wav")
	a.Spawn("Grower", "grow-task", "", "", 0, nil)
	until("limit.wav")
}
