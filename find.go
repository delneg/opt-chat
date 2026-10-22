package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The page's find (Ctrl/Cmd+F, /find): the messages of the whole log that
// hold some words, newest first, a page at a time, and the log around one
// of them, so the page can show a message tens of thousands back without
// paging back through all that lies between. The log is in memory: a find
// reads all of it: 25 ms or so on a log of 35,000 messages, 25 MB.

const (
	findPage   = 30  // matches a find answers at once
	aroundSide = 100 // messages around reads on each side of its message
)

// findTerms are what a message must hold to match text: each word, and
// each "quoted phrase" as it is written, in any case and any order.
func findTerms(text string) []string {
	var terms []string
	add := func(s string) {
		if s = strings.Join(strings.Fields(s), " "); s != "" && !slices.Contains(terms, s) {
			terms = append(terms, s)
		}
	}
	text = strings.NewReplacer("“", `"`, "”", `"`).Replace(text) // a Mac's smart quotes
	for i := 0; ; i++ {
		before, phrase, quoted := strings.Cut(text, `"`)
		if i%2 == 0 {
			for _, w := range strings.Fields(before) {
				add(w)
			}
		} else {
			add(before) // a quote left open runs to the end
		}
		if !quoted {
			return terms
		}
		text = phrase
	}
}

// indexFold is where s first holds sub in any case, as strings.EqualFold
// compares them, and where that ends; -1 when it does not. It looks for
// each form of sub's first character with strings.Index, which is fast,
// and compares the rest there. On a log of 35,000 messages, a regexp with
// (?i) took ten times as long, and looking for the first two characters'
// forms ("to", "To", "tO", "TO") half again as long.
func indexFold(s, sub string) (int, int) {
	if sub == "" {
		return 0, 0
	}
	r, n := utf8.DecodeRuneInString(sub)
	rest := sub[n:]
	var forms []string // r in each case
	for f := r; ; {
		forms = append(forms, string(f))
		if f = unicode.SimpleFold(f); f == r {
			break
		}
	}
	next := make([]int, len(forms)) // where each form is next found from i; -1: nowhere
	for j := range next {
		next[j] = -2 // not looked for
	}
	for i := 0; ; {
		at, size := -1, 0
		for j, f := range forms {
			if next[j] == -1 {
				continue
			}
			if next[j] < i {
				k := strings.Index(s[i:], f)
				if k < 0 {
					next[j] = -1
					continue
				}
				next[j] = i + k
			}
			if at < 0 || next[j] < at {
				at, size = next[j], len(f)
			}
		}
		if at < 0 {
			return -1, -1
		}
		if end := at + size + len(rest); end <= len(s) && strings.EqualFold(s[at+size:end], rest) {
			return at, end
		}
		i = at + 1
	}
}

func hasFold(s, sub string) bool { at, _ := indexFold(s, sub); return at >= 0 }

// matchAll reports whether s holds every term, and where the earliest of
// them starts and ends.
func matchAll(s string, terms []string) (bool, int, int) {
	first, last := -1, -1
	for _, t := range terms {
		at, end := indexFold(s, t)
		if at < 0 {
			return false, -1, -1
		}
		if first < 0 || at < first {
			first, last = at, end
		}
	}
	return true, first, last
}

// snippet is s around s[at:end], with up to before bytes ahead of it and
// after behind, never cutting a character, marked … where cut.
func snippet(s string, at, end, before, after int) string {
	from, to := max(0, at-before), min(len(s), end+after)
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	for to < len(s) && !utf8.RuneStart(s[to]) {
		to++
	}
	out := s[from:to]
	if from > 0 {
		out = "…" + out
	}
	if to < len(s) {
		out += "…"
	}
	return out
}

// found is one message a find matched.
type found struct {
	I        int       `json:"i"` // its first part, which the page shows it by
	Kind     string    `json:"kind"`
	Date     time.Time `json:"date"`
	Snip     string    `json:"snip"`              // one line around its first match
	Who      string    `json:"who,omitzero"`      // a work line's [Name], "user → Name" for what the user told one
	Imported bool      `json:"imported,omitzero"` // from a past chat
}

type foundReply struct {
	T      string   `json:"t"` // found
	Seq    int      `json:"seq"`
	Text   string   `json:"text"`
	Mine   bool     `json:"mine"`
	Before int      `json:"before"` // the page asked: matches before this message; 0, from the end
	Terms  []string `json:"terms"`  // what each match holds, for the page to mark
	Hits   []found  `json:"hits"`
	Total  int      `json:"total"` // messages matching in the whole log
	More   int      `json:"more"`  // the next page's before; 0: no more
}

// findable is whether a find looks in m, and with mine, whether the user
// wrote it: the user's messages and what they told a subagent full screen.
// Tool calls and their output are left out: a file read, a page fetched or
// a command's output would bury what was said, and they come back as JSON
// and logs; Opt's own search finds them.
func findable(m Msg, mine bool) bool {
	switch m.Kind {
	case "user":
		return true
	case "work":
		return !mine || strings.HasPrefix(m.Text, "[user → ")
	case "opt", "ai", "note":
		return !mine
	}
	return false
}

// Find answers the page's find: the messages before message before (0:
// the end) that hold every term of text, newest first, a page at a time.
// With mine, only the user's own; with mine and no text, all of them. A
// message's attachments are matched by what they hold (a voice message's
// words, a text file's), not by their marks.
func (c *Chat) Find(text string, before int, mine bool) foundReply {
	terms := findTerms(text)
	r := foundReply{T: "found", Text: text, Mine: mine, Before: before, Terms: terms, Hits: []found{}}
	if terms == nil {
		r.Terms = []string{}
		if !mine {
			return r
		}
	}
	c.mu.Lock()
	msgs := c.msgs // logged messages never change: read outside c.mu
	c.mu.Unlock()
	if before <= 0 || before > len(msgs) {
		before = len(msgs)
	}
	last := -1 // the last message matched: a long one's parts count once
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if !findable(m, mine) {
			continue
		}
		s := m.Text
		if strings.Contains(s, "<attachment ") {
			s = unframed(s)
		}
		ok, at, end := matchAll(s, terms)
		if !ok {
			continue
		}
		first := i
		for first > 0 && msgs[first].Part > 0 {
			first--
		}
		if first == last {
			continue
		}
		last = first
		r.Total++
		switch {
		case first >= before || r.More > 0:
		case len(r.Hits) == findPage: // one more: there is a next page
			r.More = r.Hits[findPage-1].I
		default:
			h := found{I: first, Kind: m.Kind, Date: msgs[first].Date, Imported: m.Imported}
			h.Snip = strings.Join(strings.Fields(snippet(s, at, end, 70, 160)), " ")
			if m.Kind == "work" && strings.HasPrefix(m.Text, "[") {
				h.Who, _, _ = strings.Cut(m.Text[1:], "]")
				if rest, ok := strings.CutPrefix(h.Snip, "["+h.Who+"]"); ok {
					h.Snip = strings.TrimSpace(rest)
				}
			}
			r.Hits = append(r.Hits, h)
		}
	}
	return r
}

type aroundReply struct {
	T      string  `json:"t"` // around
	ID     int     `json:"id"`
	Msgs   []Msg   `json:"msgs"`
	Asides []Aside `json:"asides"`
	Total  int     `json:"total"`
	Error  string  `json:"error,omitzero"`
}

// around is the log around message id, whole messages from aroundSide
// before it to aroundSide after: what the page shows of a find's match.
func (c *Chat) around(id int) aroundReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := aroundReply{T: "around", ID: id, Msgs: []Msg{}, Asides: []Aside{}, Total: len(c.msgs)}
	if id < 0 || id >= len(c.msgs) {
		r.Error = fmt.Sprintf("No message %d: the chat has %d.", id, len(c.msgs))
		return r
	}
	from, to := c.whole(max(0, id-aroundSide)), c.wholeEnd(id+aroundSide+1)
	r.Msgs, r.Asides = c.msgs[from:to], c.asidesIn(from, to)
	return r
}

// newer is the page of the log after message after, for a page that shows
// the past and scrolls down toward the end.
func (c *Chat) newer(after int) historyReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := min(max(after+1, 0), len(c.msgs))
	to := c.wholeEnd(from + page)
	return historyReply{T: "newer", Msgs: c.msgs[from:to], Asides: c.asidesIn(from, to)}
}

// wholeEnd moves i on past the last part of the message before it, so that
// a page of the log ends with a whole one, and keeps it in the log. Call
// with c.mu held.
func (c *Chat) wholeEnd(i int) int {
	i = min(i, len(c.msgs))
	for i > 0 && i < len(c.msgs) && c.msgs[i].Part > 0 {
		i++
	}
	return i
}
