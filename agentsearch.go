package main

import (
	"bytes"
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/samber/lo"
)

const (
	searchAgentsMax = 8 // subagents a search names, the newest first
	searchAgentHits = 3 // zooms it gives for each, its newest hits
)

// searchAgents is search's part in the subagents' chats: a line for each
// that holds text, the latest at work first, with its count of hits, a
// snippet of the newest, and the zooms that open the newest few; "" when
// none does, or with -agent-memory=false. It reads their logs as feed
// reads one, whole steps only, holding the subagent's lock as a write
// does, and counts message ids as feed logs them, a step split into parts
// as logMsgs splits it. It opens no chat: an opened chat builds its
// lines, and only a zoom should pay for that.
func (a *Agent) searchAgents(text string) string {
	text = strings.TrimSpace(text)
	if a.mems == nil || text == "" {
		return ""
	}
	logs, _ := filepath.Glob(filepath.Join(a.chat.dir, "agents", "*.log"))
	type found struct {
		name  string
		at    time.Time
		n     int
		ids   []int // the newest first
		first string
	}
	var all []found
	for _, path := range logs {
		name := strings.TrimSuffix(filepath.Base(path), ".log")
		am := a.mems.lock(name) // as a zoom's feed: no step half written
		info, err := os.Stat(path)
		b, err2 := os.ReadFile(path)
		am.mu.Unlock()
		if err != nil || err2 != nil {
			continue
		}
		if j := bytes.LastIndex(b, []byte("\n\n")); j < 0 || !hasFold(string(b[:j]), text) { // most logs hold none: skip parsing them
			continue
		} else {
			b = b[:j+2]
		}
		var msgs []Msg
		for _, s := range parseSteps(b) {
			if s.Kind != "thinking" {
				for _, p := range splitText(s.Text, clipMax) {
					msgs = append(msgs, Msg{Kind: s.Kind, Text: p, Ref: s.Ref})
				}
			}
		}
		lookups := lookupRefs(msgs)
		f := found{name: name, at: info.ModTime()}
		for i, m := range slices.Backward(msgs) {
			if lookups[m.Ref] || !hasFold(m.Text, text) {
				continue
			}
			if f.n++; f.n == 1 {
				at, end := indexFold(m.Text, text)
				f.first = m.Kind + ": " + flat(snippet(m.Text, at, end, 40, 60))
			}
			if len(f.ids) < searchAgentHits {
				f.ids = append(f.ids, i)
			}
		}
		if f.n > 0 {
			all = append(all, f)
		}
	}
	a.mems.closeIdle("") // the entries made for logs no chat is open for
	if len(all) == 0 {
		return ""
	}
	slices.SortFunc(all, func(x, y found) int { return cmp.Or(y.at.Compare(x.at), strings.Compare(x.name, y.name)) })
	var b strings.Builder
	b.WriteString("\nSubagents' chats, the latest at work first:")
	for _, f := range all[:min(len(all), searchAgentsMax)] {
		zooms := make([]string, len(f.ids))
		for i, id := range f.ids {
			zooms[i] = fmt.Sprintf("zoom(%q, 1)", fmt.Sprintf("%s:%d", f.name, id))
		}
		fmt.Fprintf(&b, "\nalso in [%s]: %s, the newest %s; open: %s", f.name, count(f.n, "hit"), f.first, strings.Join(zooms, ", "))
	}
	if more := len(all) - searchAgentsMax; more > 0 {
		fmt.Fprintf(&b, "\nand %d more subagents hold it: %s.", more,
			strings.Join(lo.Map(all[searchAgentsMax:min(len(all), searchAgentsMax+20)], func(f found, _ int) string { return f.name }), ", "))
	}
	return b.String()
}
