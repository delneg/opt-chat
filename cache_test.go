package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
)

// fakeCache caches as Claude Code's calls were measured to: the system
// prompt and the last block of each of the last two messages are marked; a
// mark reads back the longest prefix cached at the end of a block up to 20
// blocks before it, in its message or an earlier one (a block that grew is
// a new block, and nothing reads its old end); and a prefix is cached once a
// request that marks it has begun its response (no TTL). Measured on Haiku
// and Sonnet 5.5 through Claude Code 2.1: a first message sent alone and
// answered "ok" is read back by a later call that sends it as the first
// block of a longer message, and by none that sends it grown.
// (After https://github.com/VictorTaelin/UniiChat/blob/main/cache.test.ts.)
type fakeCache struct {
	mu   sync.Mutex
	live map[[32]byte]bool
	use  map[string]*cacheUse
}

// cacheUse is what requests of a kind read from the cache and wrote to it,
// in bytes, how many there were and how many of them were acks; own is the
// bytes of compactions' tasks, theirs alone, which share leaves out.
type cacheUse struct{ read, write, own, n, acks int }

func (u cacheUse) share() float64 { return float64(u.read) / float64(max(1, u.read+u.write)) }

// dollars is what the requests cost at Sonnet's prices ($3 a million input
// tokens; writes 1.25 times that, reads 0.1), at 2.5 bytes a token, with
// the system prompt and tools (7k tokens, as measured) read by every
// request, and 4 tokens of output (5 times input's price) for each ack.
func (u cacheUse) dollars() float64 {
	tokens := (0.1*float64(u.read)+1.25*float64(u.write+u.own))/2.5 + float64(u.n)*0.1*7000 + float64(u.acks)*4*5
	return tokens * 3e-6
}

// ask is one request of messages, each of blocks, own bytes of the last its
// own (a task); ack: the request is an ack.
func (f *fakeCache) ask(kind string, msgs [][]string, own int, ack bool) {
	var keys [][32]byte
	var ends, marks []int
	h, end := sha256.Sum256([]byte("system")), 0
	for i, m := range msgs {
		h = sha256.Sum256(append(h[:], "\x01"...)) // a message begins
		for _, b := range m {
			h = sha256.Sum256(append(h[:], b...))
			end += len(b)
			keys, ends = append(keys, h), append(ends, end)
		}
		if i >= len(msgs)-2 {
			marks = append(marks, len(keys)-1)
		}
	}
	f.mu.Lock()
	read := 0
	for _, m := range marks {
		for b := m; b >= max(0, m-20); b-- {
			if f.live[keys[b]] {
				read = max(read, ends[b])
				break
			}
		}
	}
	u := f.use[kind]
	if u == nil {
		u = &cacheUse{}
		f.use[kind] = u
	}
	u.read, u.write, u.own, u.n = u.read+read, u.write+end-read-own, u.own+own, u.n+1
	if ack {
		u.acks++
	}
	f.mu.Unlock()
	time.Sleep(200 * time.Microsecond) // its response begins
	f.mu.Lock()
	for _, m := range marks {
		f.live[keys[m]] = true
	}
	f.mu.Unlock()
}

// texts renders blocks as the cache keys them.
func texts(bs []block) []string { return lo.Map(bs, func(b block, _ int) string { return b.Text }) }

// call makes a call's requests as Agent.ack and the call after it do: the
// first message alone, written once while others wait and noted cached as
// its response begins, then with the rest. Its kind stands for its prefix.
func (f *fakeCache) call(a *Agent, kind string, first, second []block, own int) {
	if first == nil {
		f.ask(kind, [][]string{texts(second)}, own, false)
		return
	}
	release := a.writeOnce(context.Background(), kind+"\x00"+first[0].Text)
	f.ask(kind, [][]string{texts(first)}, 0, true)
	a.noteCached(kind + "\x00" + first[0].Text)
	release()
	f.ask(kind, [][]string{texts(first), {"ok"}, texts(second)}, own, false)
}

// compact makes a compaction's requests as Agent.Compact does.
func (f *fakeCache) compact(a *Agent, lines []string, stable int, task []block, own int) {
	first, rest := a.compactView(context.Background(), "compaction", lines, stable, task)
	f.call(a, "compaction", first, rest, own)
}

// A few thousand messages, with an import of 1,500 in the middle and the
// chat going on while it is summarized, replayed through the chat's own
// scheduling, views and compaction inputs against fakeCache: turns and
// compactions read most of their input from the cache, no compaction's
// context passes the compaction view's cap, and each turn sees the latest
// exchange after the import as its own lines. UniiChat asks 95% of each;
// with no lookback into a message that grew, a turn writes the lines after
// its first message's checkpoint anew, and turns here read 67 to 78%,
// compactions acked every time 83 to 88%. Before compactions' contexts
// ended at the first message not summarized (2026-10-20), they read 37%,
// one saw 527 KB of context, and 42 turns lost the latest reply into a
// merged line. Sending a cached first message with the rest rather than
// acking it again (compactView) costs compactions about 17% less here, and
// about 20% less replaying a real chat's import.
func TestCacheReplay(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("logs and builds about 4,000 lines")
	}
	c, _ := fakeChat(t, t.TempDir(), 0)
	cache := &fakeCache{live: map[[32]byte]bool{}, use: map[string]*cacheUse{}}
	a := &Agent{writing: map[string]chan struct{}{}}
	// Every compaction acked, as before 2026-10-20, against a cache of its own.
	acked := &fakeCache{live: map[[32]byte]bool{}, use: map[string]*cacheUse{}}
	a0 := &Agent{writing: map[string]chan struct{}{}}
	rng := rand.New(rand.NewPCG(1, 2))
	words := func(lo, hi int) string { // a text of lo to hi bytes
		n := lo + rng.IntN(hi-lo+1)
		b := make([]byte, n)
		for i := range b {
			b[i] = "abcdefghij klmnopqrst uvwxyz"[rng.IntN(28)]
		}
		return string(b)
	}
	var mu sync.Mutex
	most := 0
	c.mu.Lock()
	c.compact = func(_ context.Context, lines []string, stable int, task []block) (string, error) {
		size := lo.SumBy(lines, func(l string) int { return len(l) + 1 })
		mu.Lock()
		most = max(most, size)
		mu.Unlock()
		own := lo.SumBy(task, func(b block) int { return len(b.Text) })
		var wg sync.WaitGroup
		wg.Go(func() { cache.compact(a, lines, stable, task, own) })
		wg.Go(func() {
			first, second := splitView(lines, stable, ccheckpoint, task)
			acked.call(a0, "compaction", first, second, own)
		})
		wg.Wait()
		return strings.Repeat("s", 300+len(lines)%200), nil
	}
	c.mu.Unlock()
	missed := 0         // turns whose view showed the latest reply in a line with others
	imported := 1 << 30 // the end of the import, once logged: the chat's own lines after it
	turn := func(i int) {
		if err := c.settle(context.Background(), func(int, int) {}); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		lines := c.callView()
		lastOpt := -1
		for j := len(c.msgs) - 1; j >= 0 && lastOpt < 0; j-- {
			if c.msgs[j].Kind == "opt" {
				lastOpt = j
			}
		}
		c.mu.Unlock()
		if lastOpt >= imported && !lo.ContainsBy(lines, func(l string) bool { return strings.HasPrefix(l, fmt.Sprintf("%d+1|", lastOpt)) }) {
			missed++
		}
		user := fmt.Sprintf("question %d %s", i, words(40, 1500))
		c.Log("user", user)
		first, second := viewMessages(lines, []block{textBlock(user)})
		cache.call(a, "turn", first, second, 0)
		var ms []Msg
		for range rng.IntN(7) {
			ms = append(ms, Msg{Kind: "tool", Text: "Bash " + words(80, 600)}, Msg{Kind: "echo", Text: words(40, 8000)})
		}
		c.logMsgs(append(ms, Msg{Kind: "opt", Text: words(100, 2500)}))
	}
	n := 0
	for c.logLen() < 1500 {
		turn(n)
		n++
	}
	var imp []Msg
	for i := range 1500 {
		kind := lo.Ternary(i%2 == 0, "user", kindAI)
		text := lo.Ternary(rng.IntN(5) < 3, words(20, 400), words(600, 3000)) // most free, as an import's are
		imp = append(imp, Msg{Kind: kind, Text: text, Imported: true})
	}
	c.logMsgs(imp)
	imported = c.logLen()
	for c.logLen() < 4000 {
		turn(n)
		n++
	}
	waitIdle(t, c)
	for kind, u := range cache.use {
		t.Logf("%s: %d requests (%d acks), %.1f%% read, $%.2f", kind, u.n, u.acks, 100*u.share(), u.dollars())
	}
	was := acked.use["compaction"]
	t.Logf("compactions acked every time: %d requests, %.1f%% read, $%.2f", was.n, 100*was.share(), was.dollars())
	if missed > 0 {
		t.Errorf("%d turns after the import did not see the latest reply as its own line", missed)
	}
	if most > cviewHi {
		t.Errorf("a compaction saw %d bytes of context, over %d", most, cviewHi)
	}
	if s := cache.use["turn"].share(); s < 0.65 {
		t.Errorf("turns read %.1f%% from the cache", 100*s)
	}
	if s := was.share(); s < 0.78 {
		t.Errorf("compactions acked every time read %.1f%% from the cache", 100*s)
	}
	if now := cache.use["compaction"].dollars(); now > 0.9*was.dollars() {
		t.Errorf("compactions cost $%.2f, against $%.2f acked every time", now, was.dollars())
	}
}

func (c *Chat) logLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}

// A compaction whose first message, or one up to creuse lines shorter, is
// cached sends it with the rest of the view in one message: the same text,
// split elsewhere. One that a call is writing waits for that call's
// response to begin; one cached longer ago than Claude Code keeps it, or
// further back, is written anew.
func TestCompactView(t *testing.T) {
	t.Parallel()
	a := &Agent{writing: map[string]chan struct{}{}}
	lines := make([]string, 80)
	for i := range lines {
		lines[i] = fmt.Sprintf("%d+1|user: line %d", i, i)
	}
	task := []block{textBlock("Compaction: x")}
	first, _ := a.compactView(t.Context(), "m", lines[:41], 41, task)
	if first == nil || !strings.Contains(first[0].Text, lines[39]+"\n</chat>") {
		t.Fatalf("no first message to write: %v", first)
	}
	a.noteCached("m\x00" + first[0].Text)
	if f, _ := a.compactView(t.Context(), "n", lines[:41], 41, task); f == nil {
		t.Fatal("another model read the first message cached for this one")
	}
	f, r := a.compactView(t.Context(), "m", lines[:50], 50, task) // cut at 48, a checkpoint past the cached one
	if f != nil || len(r) != 4 || r[0].Text != first[0].Text || r[1].Text != viewHere || r[3].Text != "Compaction: x" {
		t.Fatalf("the cached first message was not sent with the rest: %v / %v", f, r)
	}
	if r[2].Text != "<chat>\n"+strings.Join(lines[40:50], "\n")+"\n</chat>" {
		t.Fatalf("the rest of the view: %q", r[2].Text)
	}
	if f, _ := a.compactView(t.Context(), "m", lines[:58], 58, task); f == nil { // two checkpoints past it
		t.Fatal("a first message far behind was used")
	}
	if f, _ := a.compactView(t.Context(), "m", lines[:41], 30, task); f == nil {
		t.Fatal("a first message past the stable lines was used")
	}
	a.mu.Lock()
	for k := range a.cached {
		a.cached[k] = time.Now().Add(-cachedFor)
	}
	a.mu.Unlock()
	if f, _ := a.compactView(t.Context(), "m", lines[:41], 41, task); f == nil {
		t.Fatal("a first message cached too long ago was used")
	}
	// Another call writing it: wait for its response to begin, then read it.
	release := a.writeOnce(t.Context(), "m\x00"+first[0].Text)
	done := make(chan []block)
	go func() {
		f, _ := a.compactView(t.Context(), "m", lines[:44], 44, task)
		done <- f
	}()
	select {
	case <-done:
		t.Fatal("went on while another call wrote the first message")
	case <-time.After(50 * time.Millisecond):
	}
	a.noteCached("m\x00" + first[0].Text) // its response begins, as ack has it
	release()
	if f := <-done; f != nil {
		t.Fatal("wrote the first message another call had just written")
	}
}

// An ack notes its first message cached as its response begins, and only
// then: a call that waited on one that failed before it writes it anew.
func TestAckNotesCached(t *testing.T) {
	t.Parallel()
	a := &Agent{writing: map[string]chan struct{}{}}
	ack := func(out ...string) error {
		p := &proc{prefix: "m", ctx: t.Context(), out: bufio.NewScanner(strings.NewReader(strings.Join(out, "\n")))}
		return a.ack(p, []block{textBlock("view")}, &usage{}, nil)
	}
	if err := ack(`{"type":"result","is_error":true,"result":"overloaded"}`); err == nil || a.inCache(t.Context(), "m\x00view") {
		t.Fatalf("a failed ack left its first message cached: %v", err)
	}
	if err := ack(`{"type":"stream_event","event":{"type":"message_start"}}`, `{"type":"result","result":"ok"}`); err != nil || !a.inCache(t.Context(), "m\x00view") {
		t.Fatalf("an ack's first message not cached: %v", err)
	}
	// A call waiting on a writer that fails finds nothing cached.
	release := a.writeOnce(t.Context(), "m\x00other")
	done := make(chan bool)
	go func() { done <- a.inCache(t.Context(), "m\x00other") }()
	time.Sleep(20 * time.Millisecond)
	release()
	if <-done {
		t.Fatal("a writer that failed left its first message cached")
	}
}
