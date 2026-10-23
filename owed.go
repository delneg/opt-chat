package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The owed list (-owed): what the user asked for and Opt hasn't done, kept
// in <dir>/owed.jsonl, outside memory. Summaries used to carry such lists
// from line to line, and one item no one asked for came back for a week
// (docs/2026-10-23-faithful-lines.md). Here each item cites the user's
// message that asked for it, Opt adds items with the owe tool, and only the
// user closes one, with /done. Each turn opens with the list.

const owedPrompt = `- owe(from, text) adds to your owed list a thing the user asked for in
  message from (a user's message) that you haven't done; owe() lists them.
  Each turn opens with the list. Only the user closes an item, with /done.
`

type owedItem struct {
	ID   int       `json:"id"`
	From int       `json:"from"` // the user's message that asked for it
	Text string    `json:"text"`
	Set  time.Time `json:"set"`
}

type owedList struct {
	path string
	mu   sync.Mutex
	list []owedItem
}

func loadOwed(dir string) (*owedList, error) {
	o := &owedList{path: filepath.Join(dir, "owed.jsonl")}
	b, err := os.ReadFile(o.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for line := range bytes.Lines(b) {
		var it owedItem
		if json.Unmarshal(bytes.TrimSpace(line), &it) == nil {
			o.list = append(o.list, it)
		}
	}
	return o, nil
}

func (o *owedList) saveLocked() error {
	var b []byte
	for _, it := range o.list {
		line, _ := json.Marshal(it)
		b = append(append(b, line...), '\n')
	}
	return writeAtomic(o.path, b)
}

func (o *owedList) add(from int, text string) (owedItem, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	id := 1
	for _, it := range o.list {
		id = max(id, it.ID+1)
	}
	it := owedItem{ID: id, From: from, Text: text, Set: time.Now()}
	o.list = append(o.list, it)
	return it, o.saveLocked()
}

func (o *owedList) close(id int) (owedItem, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := slices.IndexFunc(o.list, func(it owedItem) bool { return it.ID == id })
	if n < 0 {
		return owedItem{}, false, nil
	}
	it := o.list[n]
	o.list = slices.Delete(o.list, n, n+1)
	return it, true, o.saveLocked()
}

// text is the list, one item a line, or "" when it is empty.
func (o *owedList) text() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var b strings.Builder
	for _, it := range o.list {
		fmt.Fprintf(&b, "#%d (asked in %d): %s\n", it.ID, it.From, it.Text)
	}
	return b.String()
}

// owedNote opens a turn with the owed list; nil when it is off or empty.
func (a *Agent) owedNote() []block {
	a.mu.Lock()
	o := a.owed
	a.mu.Unlock()
	if o == nil {
		return nil
	}
	if t := o.text(); t != "" {
		return []block{textBlock("[opt-chat] Owed, each closed only by the user:\n" + t + "\n")}
	}
	return nil
}

func (a *Agent) oweTool(fromAgent bool, x toolArgs) string {
	a.mu.Lock()
	o := a.owed
	a.mu.Unlock()
	switch {
	case o == nil:
		return "The owed list is off: opt-chat runs with -owed=false."
	case fromAgent:
		return "Only Opt keeps the owed list."
	case x.From == 0 && strings.TrimSpace(x.Text) == "":
		if t := o.text(); t != "" {
			return strings.TrimSuffix(t, "\n")
		}
		return "Nothing owed."
	case strings.TrimSpace(x.Text) == "":
		return "No text given."
	}
	c := a.chat
	c.mu.Lock()
	ok := x.From >= 0 && x.From < len(c.msgs) && c.msgs[x.From].Kind == "user"
	c.mu.Unlock()
	if !ok {
		return fmt.Sprintf("Message %d is no message of the user's: an owed item cites the user's message that asked for it.", x.From)
	}
	it, err := o.add(x.From, strings.TrimSpace(x.Text))
	if err != nil {
		return "owe: " + err.Error()
	}
	return fmt.Sprintf("Owed #%d added (asked in %d).", it.ID, it.From)
}

// owedCommand is /owed, which lists the items, and /done N, which closes one.
func owedCommand(a *Agent, word, arg string) string {
	a.mu.Lock()
	o := a.owed
	a.mu.Unlock()
	if o == nil {
		return "The owed list is off: opt-chat runs with -owed=false."
	}
	if word == "owed" {
		if t := o.text(); t != "" {
			return strings.TrimSuffix(t, "\n")
		}
		return "Nothing owed."
	}
	id, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
	if err != nil {
		return "/done takes an item's number, as /done 3; /owed lists them."
	}
	it, ok, err := o.close(id)
	switch {
	case err != nil:
		return "done: " + err.Error()
	case !ok:
		return fmt.Sprintf("No owed item #%d.", id)
	}
	return fmt.Sprintf("Closed #%d: %s", it.ID, it.Text)
}
