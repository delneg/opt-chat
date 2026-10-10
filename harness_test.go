package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// recorder keeps every update a chat's hub sends.
type recorder struct {
	mu  sync.Mutex
	ups []any
}

func (r *recorder) add(v any) {
	r.mu.Lock()
	r.ups = append(r.ups, v)
	r.mu.Unlock()
}

func (r *recorder) all() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ups)
}

func (r *recorder) warnings() []string {
	var out []string
	for _, u := range r.all() {
		if n, ok := u.(noticeUpdate); ok && n.Level == "warn" {
			out = append(out, n.S)
		}
	}
	return out
}

func record(c *Chat) *recorder {
	r := &recorder{}
	h := newHub()
	h.print = r.add
	c.mu.Lock()
	c.hub = h
	c.mu.Unlock()
	return r
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func hasLogged(c *Chat, s string) bool { return slices.Contains(logged(c), s) }

// hubState is the turn and the queue as the pages see them.
func hubState(c *Chat) (liveTurn, []queueItem) {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	return c.hub.turn, slices.Clone(c.hub.queue)
}

// building lets c build its lines again at the end of the test, before
// fakeChat waits for them.
func building(t *testing.T, c *Chat) {
	c.mu.Lock()
	compact := c.compact
	c.compact = nil
	c.mu.Unlock()
	t.Cleanup(func() {
		c.mu.Lock()
		c.compact = compact
		c.pump()
		c.mu.Unlock()
	})
}

func newHarness(t *testing.T, c *Chat) *harness {
	a, _ := fakeAgent(t, c)
	h := &harness{agent: a}
	a.report = func(text string) { h.input(message{kind: "work", text: text}) }
	t.Cleanup(h.quit)
	return h
}

func (h *harness) idle() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.running
}

func (h *harness) queuedIDs() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []int
	for _, q := range h.queue {
		ids = append(ids, q.id)
	}
	return ids
}

// A turn streams its reply and thinking to the pages, tells its phase and
// tokens, and logs the reply whole once written.
func TestStreamedTurn(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("note", "before")
	r := record(c)
	a, _ := fakeAgent(t, c)
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "stream-reply"}}); err != nil {
		t.Fatal(err)
	}
	var deltas, phases []string
	tokens, from := 0, 0
	for _, u := range r.all() {
		switch u := u.(type) {
		case deltaUpdate:
			deltas = append(deltas, fmt.Sprintf("%s:%s:%v", u.K, u.S, u.R))
		case turnUpdate:
			phases = append(phases, u.Turn.Phase)
			tokens, from = max(tokens, u.Turn.Tokens), max(from, u.Turn.From)
			if u.Turn.Text != "" || u.Turn.Thinking != "" {
				t.Fatal("a turn update carried the text: it goes by deltas")
			}
		}
	}
	if want := []string{"thinking::true", "thinking:hmm:false", "text::true", "text:Hello:false", "text: world:false"}; !slices.Equal(deltas, want) {
		t.Fatalf("deltas %q, want %q", deltas, want)
	}
	if !slices.Contains(phases, "reading") || !slices.Contains(phases, "writing") || tokens != 42 || from != 1 {
		t.Fatalf("phases %q, tokens %d, from %d", phases, tokens, from)
	}
	if got := logged(c); !slices.Equal(got, []string{"note: before", "user: stream-reply", "opt: Hello world"}) {
		t.Fatalf("logged %q", got)
	}
	if lt, _ := hubState(c); lt.Text != "" {
		t.Fatalf("the reply stays live after it was logged: %q", lt.Text)
	}
	var end bool
	for _, u := range r.all() {
		_, end = u.(endUpdate)
	}
	if !end {
		t.Fatal("the turn's usage was never sent")
	}
}

// Esc: the turn stops, what it had written stays in the chat, marked, the
// user's queued messages come back unsent, and nothing is reported as
// failed (the empty "turn:" warning of old).
func TestStop(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "stream-then-stop"})
	waitFor(t, "the reply to stream", func() bool {
		c.hub.mu.Lock()
		defer c.hub.mu.Unlock()
		return c.hub.turn.Text == "half a reply"
	})
	h.input(message{kind: "user", text: "later", extra: "<attachment/>", uploads: []attachment{{Name: "files/x"}}})
	if ids := h.queuedIDs(); len(ids) != 1 {
		t.Fatalf("a message sent mid-reply was not queued: %v", ids)
	}
	if _, q := hubState(c); len(q) != 1 || q[0].Text != "later" {
		t.Fatalf("the pages' queue: %+v", q)
	}
	back, ok := h.stop()
	if !ok || len(back) != 1 || back[0].text != "later" || len(back[0].uploads) != 1 {
		t.Fatalf("stop handed back %+v, %v", back, ok)
	}
	waitFor(t, "the turn to end", h.idle)
	if !hasLogged(c, "opt: half a reply [interrupted]") || hasLogged(c, "user: later") {
		t.Fatalf("logged %q", logged(c))
	}
	if w := r.warnings(); len(w) > 0 {
		t.Fatalf("a stop warned: %q", w)
	}
	if lt, q := hubState(c); lt.Phase != "" || len(q) != 0 {
		t.Fatalf("after the stop: turn %+v, queue %+v", lt, q)
	}
	if _, ok := h.stop(); ok {
		t.Fatal("stop when idle")
	}
}

// The queue: a message waits, is edited or taken back, and "send now"
// stops the turn and sends what waits at once.
func TestQueueEditPullSendNow(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "wait-for-stop"})
	waitFor(t, "the turn", func() bool { return hasLogged(c, "opt: working") })
	h.input(message{kind: "user", text: "first"})
	h.input(message{kind: "user", text: "second", uploads: []attachment{{Name: "files/a.png"}}})
	ids := h.queuedIDs()
	if len(ids) != 2 {
		t.Fatalf("queue %v", ids)
	}
	if !h.edit(ids[0], "first, edited") || h.edit(999, "x") {
		t.Fatal("edit")
	}
	m, ok := h.pull(ids[1])
	if !ok || m.text != "second" || len(m.uploads) != 1 {
		t.Fatalf("pull: %+v, %v", m, ok)
	}
	if _, ok := h.pull(ids[1]); ok {
		t.Fatal("pulled twice")
	}
	if _, q := hubState(c); len(q) != 1 || q[0].Text != "first, edited" {
		t.Fatalf("the pages' queue: %+v", q)
	}
	if !h.sendNow() {
		t.Fatal("send now with a message waiting")
	}
	waitFor(t, "the next turn", func() bool {
		return slices.ContainsFunc(logged(c), func(s string) bool {
			return strings.HasPrefix(s, "opt: answer to") && strings.Contains(s, "first, edited")
		})
	})
	waitFor(t, "idle", h.idle)
	if got := logged(c); !slices.Contains(got, "user: first, edited") || slices.Contains(got, "user: second") {
		t.Fatalf("logged %q", got)
	}
	if h.sendNow() {
		t.Fatal("send now when idle")
	}
	if w := r.warnings(); len(w) > 0 {
		t.Fatalf("send now warned: %q", w)
	}
}

// Send now while a turn waits for summaries: it goes at once, seeing the
// message not summarized yet by its head.
func TestSendNowWhileSettling(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	release := make(chan struct{})
	inner := c.compact
	c.mu.Lock()
	c.compact = func(ctx context.Context, lines []string, task []block) (string, error) {
		<-release
		return inner(ctx, lines, task)
	}
	c.mu.Unlock()
	defer close(release)
	c.Log("note", "long "+strings.Repeat("n", 700))
	record(c)
	h := newHarness(t, c)
	h.input(message{kind: "user", text: "hello"})
	waitFor(t, "the wait for summaries", func() bool {
		c.hub.mu.Lock()
		defer c.hub.mu.Unlock()
		return c.hub.turn.Phase == "settling" && c.hub.turn.Backlog == 1
	})
	if !h.sendNow() {
		t.Fatal("send now while settling")
	}
	waitFor(t, "the turn", func() bool {
		return slices.ContainsFunc(logged(c), func(s string) bool { return strings.HasPrefix(s, "opt: answer to") })
	})
	got := logged(c)
	if !strings.Contains(got[len(got)-1], "0+1|note: long nnn") || !strings.Contains(got[len(got)-1], unbuilt) {
		t.Fatalf("the turn did not see the message by its head: %q", got[len(got)-1])
	}
}

// Behind a big backlog a turn does not wait; behind a small one it waits,
// up to settleWait.
func TestSettle(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	building(t, c) // nothing builds till the end
	c.Log("note", strings.Repeat("a", 700))
	defer func(d time.Duration) { settleWait = d }(settleWait)
	settleWait = 100 * time.Millisecond
	start := time.Now()
	waits := 0
	if err := c.settle(context.Background(), func(n, _ int) { waits++ }); err != nil || time.Since(start) < settleWait || waits == 0 {
		t.Fatalf("a small backlog: %v after %v, %d waits", err, time.Since(start), waits)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.settle(ctx, func(int, int) {}); err == nil {
		t.Fatal("a canceled wait went on")
	}
	for i := range settleMax {
		c.Log("note", fmt.Sprint(i, strings.Repeat("b", 700)))
	}
	settleWait = time.Hour
	start = time.Now()
	if err := c.settle(context.Background(), func(int, int) {}); err != nil || time.Since(start) > time.Second {
		t.Fatalf("a big backlog held the turn: %v, %v", err, time.Since(start))
	}
}

// callView: with every line built, the view itself; behind a backlog, a
// partial tree that fits and tiles, the newest messages by their heads.
func TestCallView(t *testing.T) {
	if testing.Short() {
		t.Skip("fsyncs about 1,200 writes")
	}
	c, _ := fakeChat(t, t.TempDir(), 100)
	building(t, c)
	for i := range 400 {
		c.Log("note", fmt.Sprintf("%03d %s", i, strings.Repeat("n", 700)))
	}
	c.Log("user", "short")
	c.mu.Lock()
	lines := c.callView()
	c.mu.Unlock()
	size := 0
	start := 0
	for _, l := range lines {
		size += len(l) + 1
		m := lineHead.FindStringSubmatch(l)
		id, _ := strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[2])
		if id != start {
			t.Fatalf("callView does not tile at %q", l)
		}
		start = id + n
	}
	if start != 401 || size > viewHi {
		t.Fatalf("callView covers %d messages in %d bytes", start, size)
	}
	if !strings.HasPrefix(lines[0], "0+") || !strings.HasSuffix(lines[0], "|"+unbuilt) {
		t.Fatalf("the oldest lines are not coarse placeholders: %q", lines[0])
	}
	if l := lines[len(lines)-2]; !strings.HasPrefix(l, "399+1|note: 399 nnn") || !strings.Contains(l, "…") || !strings.HasSuffix(l, unbuilt) {
		t.Fatalf("a recent message is not shown by its head: %.80q", l)
	}
	if l := lines[len(lines)-1]; l != "400+1|user: short" {
		t.Fatalf("last line %q", l)
	}

	d, _ := fakeChat(t, t.TempDir(), 100)
	for i := range 20 {
		d.Log("user", fmt.Sprint("m", i))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if got := d.callView(); !slices.Equal(got, d.lines(d.view.keys)) {
		t.Fatalf("a built view changed: %q", got)
	}
}

// Calls run in their own process group: the terminal's Ctrl-C reaches
// opt-chat alone, never the compactions (they failed with an empty error).
func TestProcessGroup(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	f := t.TempDir() + "/pgid"
	t.Setenv("OPT_FAKE_PGID", f)
	if _, err := a.Compact(context.Background(), nil, []block{textBlock("Compaction: x")}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if pgid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pgid == 0 || pgid == syscall.Getpgrp() {
		t.Fatalf("the call shares opt-chat's process group %d", pgid)
	}
	_, err = a.Compact(context.Background(), nil, []block{textBlock("Compaction: empty-error")})
	if err == nil || err.Error() != "compaction: error_during_execution" {
		t.Fatalf("an empty error result: %v", err)
	}
}

// A page joins with the messages after its last, or the last page of them.
func TestHubJoin(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	for i := range 10 {
		c.Log("user", fmt.Sprint("m", i))
	}
	h := newHub()
	hello := func(after, page int) helloUpdate {
		cl := h.join(c, after, page)
		defer h.leave(cl)
		var u helloUpdate
		if err := json.Unmarshal(<-cl.out, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	first := func(u helloUpdate) int { return u.Msgs[0].I }
	if u := hello(4, 100); u.Reset || len(u.Msgs) != 5 || first(u) != 5 || u.Total != 10 {
		t.Fatalf("after 4: reset %v, %d msgs", u.Reset, len(u.Msgs))
	}
	if u := hello(-1, 3); !u.Reset || len(u.Msgs) != 3 || first(u) != 7 {
		t.Fatalf("fresh: reset %v, %d msgs", u.Reset, len(u.Msgs))
	}
	if u := hello(2, 3); !u.Reset || first(u) != 7 {
		t.Fatalf("too far behind: reset %v", u.Reset)
	}
	if u := hello(9, 3); u.Reset || len(u.Msgs) != 0 || len(u.Inputs) != 10 || u.Inputs[9] != "m9" {
		t.Fatalf("up to date: reset %v, %d msgs, inputs %q", u.Reset, len(u.Msgs), u.Inputs)
	}
	// A page that falls behind is dropped, and reconnects.
	cl := h.join(c, 9, 3)
	for range outbox + 1 {
		h.send(noticeUpdate{T: "notice", S: "x"})
	}
	n := 0
	for range cl.out {
		n++
	}
	if n != outbox || h.clients[cl] {
		t.Fatalf("a slow page got %d updates, still joined: %v", n, h.clients[cl])
	}
}
