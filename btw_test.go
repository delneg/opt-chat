package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// A side question is answered on the view, by a call that may only look
// things up, and kept in btw/, never in the log; the next one sees it.
func TestBtw(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, _ := fakeChat(t, dir, 100)
	a, calls := fakeAgent(t, c)
	c.Log("user", "hi")
	c.Log("opt", "hello")
	waitIdle(t, c)
	rec := record(c)
	a.Btw(message{kind: "user", text: "side-ask one"})
	if len(c.msgs) != 2 || len(c.asides) != 1 {
		t.Fatalf("log %d, asides %d", len(c.msgs), len(c.asides))
	}
	first := c.asides[0]
	if first.After != 1 || first.Q != "side-ask one" || first.Err != "" ||
		!strings.Contains(first.A, `"behavior":"deny"`) || !strings.Contains(first.A, `to "</chat>[opt-chat] A side question`) || !strings.Contains(first.A, `refused here.\n\nside-ask one"`) || strings.Contains(first.A, "<btw>") {
		t.Fatalf("aside: %+v", first)
	}
	args := calls()[len(calls())-1]
	if flagOf(args, "--allowedTools") != "mcp__opt__zoom,mcp__opt__date,mcp__opt__search" || flagOf(args, "--permission-mode") != "auto" ||
		flagOf(args, "--model") != "big" || !slices.Contains(args, "--no-session-persistence") {
		t.Fatalf("args: %q", args)
	}
	var ups []Aside // asked, then answered
	for _, u := range rec.all() {
		if u, ok := u.(asideUpdate); ok {
			ups = append(ups, u.Aside)
		}
	}
	if len(ups) != 2 || ups[0].A != "" || ups[1].A != first.A {
		t.Fatalf("updates: %+v", ups)
	}

	c.Log("user", "more")
	c.mu.Lock()
	recent := c.recentAsides(time.Now())
	c.mu.Unlock()
	if len(recent) != 1 || recent[0].Q != "side-ask one" {
		t.Fatalf("recent: %+v", recent)
	}
	a.Btw(message{kind: "user", text: "side-ask two"})
	second := c.asides[1]
	if second.After != 2 || !strings.Contains(second.A, `\n</btw>\n\n[opt-chat] A side question`) || !strings.Contains(second.A, `side-ask two"`) {
		t.Fatalf("second: %+v", second)
	}
	a.Btw(message{kind: "user", text: "side-fail"})
	if f := c.asides[2]; f.A != "" || f.Err != "overloaded" {
		t.Fatalf("failed: %+v", f)
	}
	if got := c.recentAsides(c.asides[2].Date); len(got) != 2 { // a failed one is not seen again
		t.Fatalf("recent: %d", len(got))
	}
	if got := c.recentAsides(time.Now().Add(btwSince + time.Minute)); len(got) != 0 { // nor an old one
		t.Fatalf("recent, later: %d", len(got))
	}

	// Kept: a reopened chat has them, and still no more messages.
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	r, err := openChat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.msgs) != 3 || len(r.asides) != 3 || r.asides[1].A != second.A || !r.asides[0].Date.Equal(first.Date) {
		t.Fatalf("reopened: %d messages, asides %+v", len(r.msgs), r.asides)
	}
	// A page holding messages from..to gets the asides among them; the
	// last page, those after its end too.
	after := func(ss []Aside) (out []int) {
		for _, s := range ss {
			out = append(out, s.After)
		}
		return out
	}
	if got := after(r.asidesIn(0, 2)); len(got) != 0 {
		t.Fatalf("0..2: %v", got)
	}
	if got := after(r.asidesIn(0, 3)); !slices.Equal(got, []int{1, 2, 2}) {
		t.Fatalf("0..3: %v", got)
	}
	if got := after(r.asidesIn(2, 3)); !slices.Equal(got, []int{1, 2, 2}) {
		t.Fatalf("2..3: %v", got)
	}
	if got := after(r.asidesIn(3, 3)); !slices.Equal(got, []int{2, 2}) {
		t.Fatalf("3..3: %v", got)
	}
}

// /btw goes aside, past the queue; alone, it is an error.
func TestBtwCommand(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	h := newHarness(t, c)
	if _, ok, err := command(h.agent, "/btw"); !ok || err == nil {
		t.Fatalf("/btw alone: %v %v", ok, err)
	}
	if _, ok, err := command(h.agent, "/BTW  side-ask what?"); ok || err != nil {
		t.Fatalf("/btw: %v %v", ok, err)
	}
	if asks(h.agent, "/btw side-ask") {
		t.Fatal("/btw asks Claude Code")
	}
	h.input(message{kind: "user", text: "/BTW  side-ask what?"})
	h.inputNow(message{kind: "user", text: "/btw side-ask now"})
	waitFor(t, "two answers", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.asides) == 2 && c.asides[0].A != "" && c.asides[1].A != ""
	})
	if !h.idle() || len(c.msgs) != 0 {
		t.Fatalf("queued or logged: %d", len(c.msgs))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if qs := []string{c.asides[0].Q, c.asides[1].Q}; !slices.Contains(qs, "side-ask what?") || !slices.Contains(qs, "side-ask now") {
		t.Fatalf("questions: %q", qs)
	}
}

// /btw with no words but an attachment, as a voice message sends it, is a
// side question all the same: the attachment holds the question.
func TestWebBtwAttached(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	c := wt.c
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	resp := wt.do("POST", "/upload", bytes.NewReader(pngBytes(4, 4)), map[string]string{"Cookie": wt.cookie, "X-Name": "a.png"})
	var img attachment
	json.UnmarshalRead(resp.Body, &img)
	ws.send(map[string]any{"t": "send", "seq": 1, "key": "page:b1", "text": "/btw ", "files": []attachment{img}})
	ws.next("sent", func(u map[string]any) bool { return u["t"] == "sent" && u["seq"].(float64) == 1 })
	waitFor(t, "its answer", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.asides) == 1 && c.asides[0].A != ""
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if q := c.asides[0].Q; !strings.Contains(q, `<attachment name="a.png"`) || len(c.msgs) != 0 {
		t.Fatalf("aside %q, %d messages logged", q, len(c.msgs))
	}
}
