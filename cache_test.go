package main

import (
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
// prompt and the ends of the last two messages are marked, an exact repeat
// of a marked prefix reads it back, and a prefix is cached once a request
// with it has been answered (no lookback for a grown message, no TTL).
// (After https://github.com/VictorTaelin/UniiChat/blob/main/cache.test.ts.)
type fakeCache struct {
	mu   sync.Mutex
	live map[[32]byte]bool
	use  map[string]*cacheUse
}

// cacheUse is what requests of a kind read from the cache, wrote to it, and
// how many there were; the system prompt is left out, as is a compaction's
// task, which is its own.
type cacheUse struct{ read, write, n int }

func (u cacheUse) share() float64 { return float64(u.read) / float64(max(1, u.read+u.write)) }

// ask is one request of messages, own bytes of its last its own (a task).
func (f *fakeCache) ask(kind string, msgs []string, own int) {
	keys := make([][32]byte, len(msgs)+1)
	ends := make([]int, len(msgs)+1)
	keys[0] = sha256.Sum256([]byte("system"))
	for j, m := range msgs {
		keys[j+1] = sha256.Sum256(append(keys[j][:], m...))
		ends[j+1] = ends[j] + len(m)
	}
	marks := []int{0, len(msgs) - 1, len(msgs)}
	f.mu.Lock()
	hit := 0
	for _, m := range marks {
		if m > 0 && f.live[keys[m]] {
			hit = m
		}
	}
	u := f.use[kind]
	if u == nil {
		u = &cacheUse{}
		f.use[kind] = u
	}
	u.read, u.write, u.n = u.read+ends[hit], u.write+ends[len(msgs)]-ends[hit]-own, u.n+1
	f.mu.Unlock()
	time.Sleep(200 * time.Microsecond) // its response begins
	f.mu.Lock()
	for _, m := range marks {
		f.live[keys[m]] = true
	}
	f.mu.Unlock()
}

// text renders blocks as the cache keys them.
func text(bs []block) string {
	return strings.Join(lo.Map(bs, func(b block, _ int) string { return b.Text }), "\x00")
}

// call makes a call's requests as Agent.ack and the call after it do: the
// first message alone, written once while others wait, then with the rest.
func (f *fakeCache) call(a *Agent, kind string, first, second []block, own int) {
	if first == nil {
		f.ask(kind, []string{text(second)}, own)
		return
	}
	release := a.writeOnce(context.Background(), text(first))
	f.ask(kind, []string{text(first)}, 0)
	release()
	f.ask(kind, []string{text(first), "ok", text(second)}, own)
}

// A few thousand messages, with an import of 1,500 in the middle and the
// chat going on while it is summarized, replayed through the chat's own
// scheduling, views and compaction inputs against fakeCache: turns and
// compactions read most of their input from the cache, no compaction's
// context passes the compaction view's cap, and each turn sees the latest
// exchange after the import as its own lines. UniiChat asks 95% of each;
// without the API's lookback, which never read back through Claude Code, a
// turn writes the lines after its first message's checkpoint anew, and
// turns here read about 70%, compactions about 83%. Before compactions'
// contexts ended at the first message not summarized (2026-10-20), they
// read 37%, one saw 527 KB of context, and 42 turns lost the latest reply
// into a merged line.
func TestCacheReplay(t *testing.T) {
	if testing.Short() {
		t.Skip("logs and builds about 4,000 lines")
	}
	c, _ := fakeChat(t, t.TempDir(), 0)
	cache := &fakeCache{live: map[[32]byte]bool{}, use: map[string]*cacheUse{}}
	a := &Agent{writing: map[string]chan struct{}{}}
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
		first, second := splitView(lines, stable, ccheckpoint, task)
		cache.call(a, "compaction", first, second, len(text(task))+1)
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
		t.Logf("%s: %d requests, %.1f%% read", kind, u.n, 100*u.share())
	}
	if missed > 0 {
		t.Errorf("%d turns after the import did not see the latest reply as its own line", missed)
	}
	if most > cviewHi {
		t.Errorf("a compaction saw %d bytes of context, over %d", most, cviewHi)
	}
	if s := cache.use["turn"].share(); s < 0.65 {
		t.Errorf("turns read %.1f%% from the cache", 100*s)
	}
	if s := cache.use["compaction"].share(); s < 0.78 {
		t.Errorf("compactions read %.1f%% from the cache", 100*s)
	}
}

func (c *Chat) logLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.msgs)
}
