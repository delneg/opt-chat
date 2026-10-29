package main

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFindTerms(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"  wobbling  ", []string{"wobbling"}},
		{"Toy box toy", []string{"Toy", "box", "toy"}}, // folded when matched, not here
		{`said "the  red   fox" twice`, []string{"said", "the red fox", "twice"}},
		{`“smart quotes” too`, []string{"smart quotes", "too"}},
		{`open "quote runs on`, []string{"open", "quote runs on"}},
		{`""`, nil},
	} {
		if got := findTerms(c.in); !slices.Equal(got, c.want) {
			t.Errorf("findTerms(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIndexFold(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		s, sub  string
		at, end int
	}{
		{"I was WOBBLING with it", "wobbling", 6, 14},
		{"toy Toy TOY", "TOY", 0, 3},
		{"no match here", "wobbling", -1, -1},
		{"tttoy", "toy", 2, 5},
		{"Привет, КАРТИНА", "картина", 14, 28},
		{"short", "shorter", -1, -1},
		{"anything", "", 0, 0},
		{"x Kelvin K", "k", 2, 3},
	} {
		at, end := indexFold(c.s, c.sub)
		if at != c.at || end != c.end {
			t.Errorf("indexFold(%q, %q) = %d, %d, want %d, %d", c.s, c.sub, at, end, c.at, c.end)
		}
	}
}

// logFind logs a made-up chat for the finds below: the user's messages,
// Opt's replies, a tool call and its output, a subagent's report and what
// the user told one, an imported AI's, and a message split in parts.
func logFind(c *Chat) {
	at := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var ms []Msg
	add := func(kind, text string) {
		ms = append(ms, Msg{Kind: kind, Text: text, Date: at.Add(time.Duration(len(ms)) * time.Minute)})
	}
	add("user", "I was wobbling with a parser in Rust")                                  // 0
	add("opt", "Wobbling is fine; the parser needs a lexer first")                       // 1
	add("tool", `Bash {"command":"grep -r wobbling ."}`)                                 // 2
	add("echo", "wobbling.rs: fn wobbling() {}")                                         // 3
	add("work", "[Rex] report: the lexer wobbling is done")                              // 4
	add("work", "[user → Rex] stop wobbling and ship the lexer")                         // 5
	add("ai", "An old chat's AI said: wobbling parser")                                  // 6
	add("user", "the red fox and the blue fox")                                          // 7
	add("user", "a fox that is red, and blue skies")                                     // 8
	add("note", "a note on wobbling")                                                    // 9
	add("user", strings.Repeat("filler ", 5000)+"the hidden wobbling word")              // 10, in parts
	add("user", `look <attachment name="wobbling.png" path="files/x.png" size="1 KB"/>`) // its mark alone
	add("user", "voice: <attachment name=\"voice 0:03.webm\" path=\"files/y.webm\" size=\"3 KB\">\nI keep wobbling with it\n</attachment>")
	c.logMsgs(ms)
}

// what the find's hits are, as "i kind who"
func hitList(r foundReply) []string {
	var out []string
	for _, h := range r.Hits {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%d %s %s", h.I, h.Kind, h.Who)))
	}
	return out
}

func TestFind(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	logFind(c)
	c.mu.Lock()
	parts := len(c.msgs)
	split := slices.IndexFunc(c.msgs, func(m Msg) bool { return m.Part > 0 })
	c.mu.Unlock()
	if split < 0 {
		t.Fatal("no message in parts")
	}
	voice := parts - 1

	r := c.Find("WOBBLING", 0, false)
	want := []string{fmt.Sprint(voice, " user"), "10 user", "9 note", "6 ai", "5 work user → Rex", "4 work Rex", "1 opt", "0 user"}
	if got := hitList(r); !slices.Equal(got, want) || r.Total != len(want) || r.More != 0 {
		t.Fatalf("find wobbling: %q (total %d, more %d), want %q", got, r.Total, r.More, want)
	}
	if h := r.Hits[4]; h.Snip != "stop wobbling and ship the lexer" {
		t.Errorf("a relay's snippet: %q", h.Snip)
	}
	if h := r.Hits[1]; !strings.HasPrefix(h.Snip, "…") || !strings.Contains(h.Snip, " filler filler ") || !strings.HasSuffix(h.Snip, "filler the hidden wobbling word") {
		t.Errorf("a long message's snippet: %q", h.Snip)
	}
	if h := r.Hits[0]; h.Snip != "voice: I keep wobbling with it" {
		t.Errorf("a voice message's snippet: %q", h.Snip)
	}
	if h := r.Hits[7]; !h.Date.Equal(time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)) || h.Snip != "I was wobbling with a parser in Rust" {
		t.Errorf("the first message: %+v", h)
	}

	// words in any order, a phrase as it is written
	if got := hitList(c.Find("blue red fox", 0, false)); !slices.Equal(got, []string{"8 user", "7 user"}) {
		t.Errorf("words: %q", got)
	}
	if got := hitList(c.Find(`"red fox"`, 0, false)); !slices.Equal(got, []string{"7 user"}) {
		t.Errorf("a phrase: %q", got)
	}
	if got := hitList(c.Find(`"red fox" wobbling`, 0, false)); got != nil {
		t.Errorf("no message holds both: %q", got)
	}

	// the user's own: their messages, and what they told a subagent
	if got := hitList(c.Find("wobbling", 0, true)); !slices.Equal(got, []string{fmt.Sprint(voice, " user"), "10 user", "5 work user → Rex", "0 user"}) {
		t.Errorf("mine: %q", got)
	}
	if r := c.Find("", 0, true); r.Total != 7 || len(r.Hits) != 7 || r.Hits[len(r.Hits)-1].Snip != "I was wobbling with a parser in Rust" {
		t.Errorf("all of mine: %q, total %d", hitList(r), r.Total)
	}
	if r := c.Find("  ", 0, false); len(r.Hits) != 0 || r.Total != 0 || r.Terms == nil {
		t.Errorf("nothing typed: %+v", r)
	}
	// before: a page from there on
	if got := hitList(c.Find("wobbling", 5, false)); !slices.Equal(got, []string{"4 work Rex", "1 opt", "0 user"}) {
		t.Errorf("before 5: %q", got)
	}
}

// A find answers 30 at a time, the next page from more, the total the
// whole log's.
func TestFindPages(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	var ms []Msg
	for i := range 75 {
		ms = append(ms, Msg{Kind: "user", Text: fmt.Sprintf("gizmo number %d", i), Date: time.Now()}, Msg{Kind: "opt", Text: "noted"})
	}
	c.logMsgs(ms)
	r := c.Find("gizmo", 0, false)
	if len(r.Hits) != findPage || r.Total != 75 || r.Hits[0].I != 148 || r.More != r.Hits[findPage-1].I {
		t.Fatalf("page 1: %d hits, total %d, first %d, more %d", len(r.Hits), r.Total, r.Hits[0].I, r.More)
	}
	r = c.Find("gizmo", r.More, false)
	if len(r.Hits) != findPage || r.Hits[0].I != 88 || r.More == 0 {
		t.Fatalf("page 2: %d hits, first %d, more %d", len(r.Hits), r.Hits[0].I, r.More)
	}
	r = c.Find("gizmo", r.More, false)
	if len(r.Hits) != 15 || r.Hits[14].I != 0 || r.More != 0 || r.Total != 75 {
		t.Fatalf("page 3: %d hits, more %d, total %d", len(r.Hits), r.More, r.Total)
	}
}

// BenchmarkFind finds in a log of 40,000 made-up messages, or in the chat
// at $OPT_CHAT_FIND_DIR: a copy, never the chat in use (it writes its view).
func BenchmarkFind(b *testing.B) {
	var c *Chat
	if dir := os.Getenv("OPT_CHAT_FIND_DIR"); dir != "" {
		var err error
		if c, err = openChat(dir); err != nil {
			b.Fatal(err)
		}
	} else {
		var err error
		if c, err = openChat(b.TempDir()); err != nil { // nothing summarizes it: a log alone
			b.Fatal(err)
		}
		words := strings.Fields("the a parser lexer token tree view line message chat page find search window scroll toy box")
		var ms []Msg
		for i := range 40_000 {
			var s strings.Builder
			for j := range 60 + i%400 {
				s.WriteString(words[(i*7+j*13)%len(words)] + " ")
			}
			ms = append(ms, Msg{Kind: []string{"user", "opt", "tool", "echo"}[i%4], Text: s.String(), Date: time.Now()})
		}
		c.logMsgs(ms)
	}
	for _, q := range []string{"wobbling", "parser lexer", `"red fox"`, "e"} {
		b.Run(q, func(b *testing.B) {
			for b.Loop() {
				c.Find(q, 0, false)
			}
		})
	}
	b.Run("search", func(b *testing.B) {
		for b.Loop() {
			c.Search("wobbling", 0)
		}
	})
}

// The page's find over its WebSocket: matches a page at a time, then the
// log around one, at the log's edges too, and the pages after it.
func TestWebFind(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	c := wt.c
	logFind(c)
	var ms []Msg
	for i := range 450 {
		ms = append(ms, Msg{Kind: "user", Text: fmt.Sprintf("line %d", i), Date: time.Now()})
	}
	ms = append(ms, Msg{Kind: "opt", Text: strings.Repeat("filler ", 5000), Date: time.Now()}) // in parts
	for i := range 50 {
		ms = append(ms, Msg{Kind: "opt", Text: fmt.Sprintf("tail %d", i), Date: time.Now()})
	}
	c.logMsgs(ms)
	c.mu.Lock()
	n := len(c.msgs)
	split := slices.IndexFunc(c.msgs, func(m Msg) bool { return m.Part > 0 })
	split2 := split + 1 + slices.IndexFunc(c.msgs[split+1:], func(m Msg) bool { return m.Part > 0 })
	c.mu.Unlock()
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))

	ws.send(map[string]any{"t": "find", "text": "wobbling", "seq": 4})
	u := ws.next("found", typ("found"))
	hits := u["hits"].([]any)
	if u["seq"].(float64) != 4 || u["total"].(float64) != 8 || len(hits) != 8 || u["more"].(float64) != 0 || u["terms"].([]any)[0] != "wobbling" {
		t.Fatalf("found: %v", u)
	}
	if h := hits[len(hits)-1].(map[string]any); h["i"].(float64) != 0 || h["kind"] != "user" || h["snip"] != "I was wobbling with a parser in Rust" || h["date"] != "2026-09-01T10:00:00Z" {
		t.Fatalf("a hit: %v", h)
	}
	ws.send(map[string]any{"t": "find", "text": "line", "mine": true, "seq": 5})
	if u := ws.next("mine", typ("found")); u["seq"].(float64) != 5 || u["total"].(float64) != 450 || len(u["hits"].([]any)) != findPage ||
		u["more"].(float64) != float64(split2-1-findPage) {
		t.Fatalf("mine, a page: seq %v total %v hits %d more %v", u["seq"], u["total"], len(u["hits"].([]any)), u["more"])
	}

	around := func(id int) map[string]any {
		t.Helper()
		ws.send(map[string]any{"t": "around", "id": id})
		return ws.next("around", typ("around"))
	}
	span := func(u map[string]any) (int, int) {
		ms := u["msgs"].([]any)
		return int(ms[0].(map[string]any)["i"].(float64)), int(ms[len(ms)-1].(map[string]any)["i"].(float64))
	}
	if from, to := span(around(200)); from != 100 || to != 300 {
		t.Errorf("around 200: %d to %d", from, to)
	}
	if from, to := span(around(0)); from != 0 || to != aroundSide {
		t.Errorf("around the first: %d to %d", from, to)
	}
	if u := around(n - 1); u["total"].(float64) != float64(n) {
		t.Errorf("around the last: total %v", u["total"])
	} else if from, to := span(u); from != n-1-aroundSide || to != n-1 {
		t.Errorf("around the last: %d to %d", from, to)
	}
	// a message in parts is whole: from its first part, to its last
	if from, _ := span(around(split + aroundSide)); from != split-1 {
		t.Errorf("around after a split message: from %d, want %d", from, split-1)
	}
	if _, to := span(around(split2 - 1 - aroundSide)); to != split2 {
		t.Errorf("around before a split message: to %d, want %d", to, split2)
	}
	for _, id := range []int{-1, n} {
		if u := around(id); len(u["msgs"].([]any)) != 0 || !strings.HasPrefix(u["error"].(string), "No message") {
			t.Errorf("around %d: %v", id, u)
		}
	}

	newer := func(after int) []any {
		t.Helper()
		ws.send(map[string]any{"t": "newer", "id": after})
		return ws.next("newer", typ("newer"))["msgs"].([]any)
	}
	if ms := newer(100); len(ms) != page || ms[0].(map[string]any)["i"].(float64) != 101 {
		t.Errorf("newer after 100: %d, from %v", len(ms), ms[0])
	}
	if ms := newer(n - 3); len(ms) != 2 {
		t.Errorf("newer at the end: %d", len(ms))
	}
	if ms := newer(n - 1); len(ms) != 0 {
		t.Errorf("newer past the end: %d", len(ms))
	}
	if ms := newer(split2 - 1 - page); len(ms) != page+1 {
		t.Errorf("newer up to a split message: %d, want its last part too", len(ms))
	}
}
