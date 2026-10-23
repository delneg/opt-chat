package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json/v2"
	"errors"

	"github.com/samber/lo"
)

var lineHead = regexp.MustCompile(`(?m)^(\d+)\+(\d+)\|`)

// fakeChat opens a chat whose compactor checks what every compaction sees
// (built lines only, none past the node) and answers with a size-byte line.
var msgHead = regexp.MustCompile(`^\d+\+1\|\w+: `) // a message's line

func fakeChat(t *testing.T, dir string, size int) (*Chat, *atomic.Int64) {
	t.Helper()
	c, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int64)
	msgRe := regexp.MustCompile(`compress message (\d+) `)
	mergeRe := regexp.MustCompile(`merge lines (\S+) and (\S+), adjacent(?s:.*)messages, (\d+) to (\d+), in more detail(?s:.*)<input>\n(.*)\n(.*)\n</input>$`)
	c.compact = func(_ context.Context, lines []string, stable int, blocks []block) (string, error) {
		calls.Add(1)
		task := blocks[0].Text
		limit := -1
		if m := msgRe.FindStringSubmatch(task); m != nil {
			limit, _ = strconv.Atoi(m[1]) // lines must end before the message
		} else if m := mergeRe.FindStringSubmatch(task); m != nil {
			limit, _ = strconv.Atoi(m[4])
			limit++ // lines may cover the merge itself
			if !strings.HasPrefix(m[5], m[1]+"|") || !strings.HasPrefix(m[6], m[2]+"|") {
				t.Errorf("merge input without its lines' heads: %q", task)
			}
		} else {
			t.Errorf("unknown task: %.80q", task)
		}
		if !strings.Contains(task, ruler) {
			t.Errorf("task without ruler")
		}
		for _, l := range lines {
			if strings.Contains(l, unbuilt) && !msgHead.MatchString(l) {
				t.Errorf("compaction saw a placeholder without its message's head: %q", l)
			}
			m := lineHead.FindStringSubmatch(l)
			id, _ := strconv.Atoi(m[1])
			n, _ := strconv.Atoi(m[2])
			if id+n > limit {
				t.Errorf("compaction for %q saw line %s+%d past its end %d", task[:40], m[1], n, limit)
			}
		}
		return strings.Repeat("s", size), nil
	}
	t.Cleanup(func() { waitIdle(t, c) }) // before the directory goes
	return c, calls
}

func waitIdle(t *testing.T, c *Chat) {
	t.Helper()
	deadline := time.After(60 * time.Second) // the race detector makes the biggest tests ten times as slow
	for {
		c.mu.Lock()
		idle := len(c.busy) == 0 && len(c.ready0) == 0 && len(c.readyM) == 0
		ch := c.changed
		c.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatal("compactor never went idle")
		}
	}
}

func TestChatTreeZoomReload(t *testing.T) {
	dir := t.TempDir()
	c, calls := fakeChat(t, dir, 100)
	runtime.GC() // the lock must survive collection
	runtime.GC()
	if _, err := openChat(dir); err == nil {
		t.Fatal("a second process got the same chat")
	}
	c.Log("user", "hi")
	c.Log("opt", "hello")
	c.Log("echo", strings.Repeat("x", 600))        // over 512: needs a model call
	c.Log("user", strings.Repeat("é", clipMax+10)) // over clipMax characters: logged as two messages
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	if err := c.settle(context.Background(), func(int, int) {}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)

	if len(c.msgs) != 5 {
		t.Fatalf("%d messages, want 5", len(c.msgs))
	}
	if got := c.tree[key{0, 0}]; got != "user: hi" {
		t.Fatalf("short message not its own line: %q", got)
	}
	if got := c.tree[key{1, 0}]; got != "user: hi\nopt: hello" {
		t.Fatalf("two short lines not joined: %q", got)
	}
	if calls.Load() == 0 || int64(c.written) != calls.Load() {
		t.Fatalf("%d model calls, %d lines written: a long message needs one", calls.Load(), c.written)
	}
	if got, _ := c.Zoom(0, 2); got != "0+1|user: hi\n1+1|opt: hello" {
		t.Fatalf("zoom(0, 2) = %q", got)
	}
	if got, _ := c.Zoom(2, 1); got != "2+1|echo: "+strings.Repeat("x", 600) {
		t.Fatalf("zoom(2, 1) = %.40q", got)
	}
	for _, bad := range [][2]int{{1, 2}, {0, 3}, {4, 8}, {-1, 1}, {5, 1}, {math.MaxInt, 1}} {
		if got, _ := c.Zoom(bad[0], bad[1]); !strings.HasPrefix(got, "No line") {
			t.Fatalf("zoom%v = %q", bad, got)
		}
	}
	if got, _ := c.Zoom(1, 2); !strings.HasSuffix(got, "The tree's roots: 0+4, 4+1.") || !strings.Contains(got, "a power of 2") {
		t.Fatalf("zoom(1, 2) does not say what would do: %q", got)
	}
	if got := c.Date(0); !strings.Contains(got, time.Now().Format("2006-01-02")) {
		t.Fatalf("date(0) = %q", got)
	}

	// A torn line (a crash mid-write) is skipped, and the next write starts on its own line.
	files, _ := filepath.Glob(filepath.Join(dir, "main", "*.jsonl"))
	if err := appendRaw(files[0], []byte(`{"i":5,"kind":"us`)); err != nil {
		t.Fatal(err)
	}
	// Reload from a copy, since this process holds the chat's lock.
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	r, err := openChat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	same := func(a, b Msg) bool {
		return a.I == b.I && a.Kind == b.Kind && a.Text == b.Text && a.Size == b.Size && a.Date.Equal(b.Date)
	}
	if !slices.EqualFunc(r.msgs, c.msgs, same) || !slices.Equal(r.view.keys, c.view.keys) || len(r.tree) != len(c.tree) {
		t.Fatalf("reload differs: %d msgs, view %v vs %v, %d nodes vs %d", len(r.msgs), r.view.keys, c.view.keys, len(r.tree), len(c.tree))
	}
	r.compact = c.compact
	r.Log("user", "after the crash")
	if r.msgs[5].Text != "after the crash" {
		t.Fatal("log after a torn line lost its message")
	}
}

// The view's sawtooth (gist §3.2): each message only appends its line, and
// once the view passes viewHi one batch merges it down to viewLo.
func TestViewSawtooth(t *testing.T) {
	if testing.Short() {
		t.Skip("fsyncs about 2,000 writes")
	}
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 400)
	batches := 0
	for i := range 700 {
		c.mu.Lock()
		prev := slices.Clone(c.view.keys)
		c.mu.Unlock()
		c.Log("user", fmt.Sprintf("%04d %s", i, strings.Repeat("m", 480))) // a free, ~500-byte line
		waitIdle(t, c)
		c.mu.Lock()
		cur, size, csize := c.view.keys, c.view.size(), c.cview.size()
		for _, v := range []view{c.view, c.cview} {
			w := v
			if w.recount(c); w.bytes != v.bytes {
				t.Fatalf("message %d: a view counted at %d bytes, measured at %d", i, v.bytes, w.bytes)
			}
		}
		c.mu.Unlock()
		if len(cur) == len(prev)+1 && slices.Equal(cur[:len(prev)], prev) {
			if size > viewHi {
				t.Fatalf("message %d: view at %d bytes, past %d, and did not merge", i, size, viewHi)
			}
		} else {
			batches++
			if size > viewLo {
				t.Fatalf("message %d: batch left the view at %d bytes, over %d", i, size, viewLo)
			}
		}
		if csize > cviewHi {
			t.Fatalf("message %d: compaction view at %d bytes", i, csize)
		}
		start := 0
		for _, k := range cur {
			if k.start() != start {
				t.Fatalf("message %d: view does not tile the chat: %v", i, cur)
			}
			start = k.end()
		}
		if start != i+1 {
			t.Fatalf("message %d: view ends at %d", i, start)
		}
	}
	if batches < 2 {
		t.Fatalf("%d batches in 700 messages of 500 bytes", batches)
	}
}

func TestViewMessages(t *testing.T) {
	lines := make([]string, 150)
	for i := range lines {
		lines[i] = fmt.Sprintf("%d+1|user: line %d", i, i)
	}
	in := func(s string) []block { return []block{textBlock(s)} }
	first, second := viewMessages(lines[:30], in("hi"))
	if first != nil || second[0].Text != "<chat>\n"+strings.Join(lines[:30], "\n")+"\n</chat>" || second[1].Text != "hi" {
		t.Fatalf("short view: %v / %v", first, second)
	}
	a, _ := viewMessages(lines[:130], in("x"))
	b, rest := viewMessages(lines[:150], in("y"))
	if len(a) != 1 || len(b) != 1 || a[0].Text != b[0].Text {
		t.Fatal("the first message changed while the view grew within a checkpoint")
	}
	if f := b[0].Text; !strings.HasSuffix(f, viewGoesOn) || !strings.Contains(f, lines[127]+"\n</chat>") {
		t.Fatalf("first message: %.60q ... %.60q", f, f[len(f)-60:])
	}
	if rest[0].Text != "<chat>\n"+strings.Join(lines[128:], "\n")+"\n</chat>" || rest[1].Text != "y" {
		t.Fatalf("second message: %v", rest)
	}
	if e, _ := viewMessages(nil, in("z")); e != nil {
		t.Fatal("empty view got a first message")
	}
}

// A message's images are kept in files/, make its line take a model call
// that sees them inside <input>, come back with zoom(id, 1), and survive a reload.
func TestImages(t *testing.T) {
	dir := t.TempDir()
	c, _ := fakeChat(t, dir, 100)
	var seen []block
	inner := c.compact
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		if strings.Contains(task[0].Text, "compress message 0 ") {
			seen = task
		}
		return inner(ctx, lines, stable, task)
	}
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	if _, err := c.saveImage([]byte("plain text")); err == nil {
		t.Fatal("saved text as an image")
	}
	name, err := c.saveImage(png)
	if again, _ := c.saveImage(png); err != nil || again != name || !strings.HasSuffix(name, ".png") {
		t.Fatalf("saveImage: %q, %q, %v", name, again, err)
	}
	c.Log("user", "look", name)
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	if err := c.settle(context.Background(), func(int, int) {}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || !strings.HasSuffix(seen[0].Text, "<input>\nuser: look\n") || seen[1].Type != "image" ||
		seen[1].Source.MediaType != "image/png" || seen[2].Text != "</input>" {
		t.Fatalf("compaction task: %v", seen)
	}
	if text, imgs := c.Zoom(0, 1); text != "0+1|user: look" || !slices.Equal(imgs, []string{name}) {
		t.Fatalf("zoom(0, 1) = %q, %v", text, imgs)
	}
	if b := c.fileBlocks([]string{"files/gone.png"}); b[0].Type != "text" {
		t.Fatalf("lost image: %v", b)
	}
	waitIdle(t, c)
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	r, err := openChat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, imgs := r.Zoom(0, 1); !slices.Equal(imgs, []string{name}) {
		t.Fatalf("reloaded zoom(0, 1) images: %v", imgs)
	}
}

// A failed compaction is tried again at the user's next message, not only on the timer.
func TestRetryAtNextMessage(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	inner := c.compact
	var fails atomic.Int64
	fails.Store(1)
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		if fails.Add(-1) >= 0 {
			return "", fmt.Errorf("down")
		}
		return inner(ctx, lines, stable, task)
	}
	c.Log("user", strings.Repeat("a", 600))
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		cooling := c.cooling(key{0, 0})
		c.mu.Unlock()
		if cooling {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failure was not noted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Log("user", "next")
	ctx, cancel := context.WithTimeout(context.Background(), retryIn/2)
	defer cancel()
	if err := c.settle(ctx, func(int, int) {}); err != nil {
		t.Fatal("not retried at the next message")
	}
}

func TestWriteOnce(t *testing.T) {
	a := &Agent{writing: map[string]chan struct{}{}}
	release := a.writeOnce(t.Context(), "p")
	other := a.writeOnce(t.Context(), "q") // another prefix never waits
	other()
	var waited atomic.Int64
	done := make(chan struct{})
	for range 3 {
		go func() {
			a.writeOnce(t.Context(), "p")() // waits for the writer, then goes on with the rest at once
			waited.Add(1)
			done <- struct{}{}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	if waited.Load() != 0 {
		t.Fatal("a call went on while another wrote its prefix")
	}
	release()
	for range 3 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("waiters stuck after the writer finished")
		}
	}
	a.writeOnce(t.Context(), "p")() // free again

	// A call stopped while it waits goes on at once.
	release = a.writeOnce(t.Context(), "p")
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	go func() { a.writeOnce(ctx, "p")(); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a stopped call still waits for the writer")
	}
}

// A backlog, as after -import: the views stay in bounds once its lines are
// built, and no compaction sees much more than the compaction view's cap.
func TestBacklog(t *testing.T) {
	if testing.Short() {
		t.Skip("fsyncs about 1,500 writes")
	}
	c, _ := fakeChat(t, t.TempDir(), 400)
	inner := c.compact
	var most atomic.Int64
	compact := func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		if n := int64(lo.SumBy(lines, func(l string) int { return len(l) + 1 })); n > most.Load() {
			most.Store(n)
		}
		return inner(ctx, lines, stable, task)
	}
	c.mu.Lock()
	c.compact = nil // importing: nothing builds
	c.mu.Unlock()
	for i := range 500 {
		c.Log("note", fmt.Sprintf("%04d %s", i, strings.Repeat("n", 600)))
	}
	c.mu.Lock()
	c.compact = compact
	c.pump()
	c.mu.Unlock()
	waitIdle(t, c)
	c.mu.Lock()
	size, csize := c.view.size(), c.cview.size()
	c.mu.Unlock()
	if size > viewHi || csize > cviewHi {
		t.Fatalf("after the backlog: view %d bytes, compaction view %d", size, csize)
	}
	if m := most.Load(); m > cviewHi {
		t.Fatalf("a compaction saw %d bytes of context", m)
	}
}

// Messages load by id, whatever day file the clock put them in, and a
// torn view.json starts the view over instead of keeping the chat shut.
func TestLoadOrder(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "main"), 0o700)
	day := func(name string, ids ...int) {
		for _, i := range ids {
			b, _ := json.Marshal(Msg{I: i, Kind: "user", Text: fmt.Sprint("m", i), Size: 8, Date: time.Now()})
			appendRaw(filepath.Join(dir, "main", name+".jsonl"), append(b, '\n'))
		}
	}
	day("2026-10-08", 0, 1)
	day("2026-10-07", 2) // the clock went back
	os.WriteFile(filepath.Join(dir, "view.json"), []byte(`[[0,0],[0,`), 0o600)
	c, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.msgs) != 3 || c.msgs[2].Text != "m2" || !slices.Equal(c.view.keys, []key{{0, 0}, {0, 1}, {0, 2}}) {
		t.Fatalf("loaded %v, view %v", c.msgs, c.view.keys)
	}
	if bad, _ := filepath.Glob(filepath.Join(dir, "view.json.bad-*")); len(bad) != 1 {
		t.Fatalf("the torn view.json was not kept aside: %v", bad)
	} else if b, _ := os.ReadFile(bad[0]); string(b) != `[[0,0],[0,` {
		t.Fatalf("kept aside: %q", b)
	}
	if !tiles([][2]int{{1, 0}, {0, 2}}, 3) || tiles([][2]int{{0, 0}, {0, 2}}, 3) || tiles([][2]int{{2, 0}}, 3) || tiles([][2]int{{99, 0}}, 3) {
		t.Fatal("tiles")
	}
}

// A batch cut short for want of parents goes on after a restart, as it
// would have without one; a view.json from before it was saved reads too.
func TestViewKeepsBatch(t *testing.T) {
	open := func(view string) *Chat {
		dir := t.TempDir()
		var msgs, nodes []any
		var keys [][2]int
		for i := range 140 { // 140 built lines of 480 bytes: past viewLo, under viewHi, with no parents
			text := fmt.Sprintf("%03d %s", i, strings.Repeat("x", 470))
			msgs = append(msgs, Msg{I: i, Kind: "user", Text: text, Size: 6 + len(text), Date: time.Now()})
			nodes = append(nodes, Node{L: 0, I: i, Text: "user: " + text, Size: 6 + len(text)})
			keys = append(keys, [2]int{0, i})
		}
		c := &Chat{dir: dir}
		os.MkdirAll(filepath.Join(dir, "main"), 0o700)
		os.MkdirAll(filepath.Join(dir, "tree"), 0o700)
		c.appendDay("main", msgs...)
		c.appendDay("tree", nodes...)
		b, _ := json.Marshal(savedView{Keys: keys, Shrinking: true})
		if view == "old" {
			b, _ = json.Marshal(keys)
		}
		os.WriteFile(filepath.Join(dir, "view.json"), b, 0o600)
		c, err := openChat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open("cut short")
	b, _ := os.ReadFile(filepath.Join(c.dir, "view.json"))
	if v, err := readView(b); !c.view.shrinking || c.view.size() <= viewLo || err != nil || !v.Shrinking || len(v.Keys) != 140 {
		t.Fatalf("the batch: %v, %d bytes; saved %s", c.view.shrinking, c.view.size(), b[:40])
	}
	if c := open("old"); c.view.shrinking || len(c.view.keys) != 140 {
		t.Fatalf("an old view.json: %v, %d lines", c.view.shrinking, len(c.view.keys))
	}
}

// An import's placeholders weigh nothing in the view: built, the lines from
// before it stay as fine as without one (lines of 64 messages here), never
// merged further for want of the space the placeholders took.
func TestImportKeepsDetail(t *testing.T) {
	if testing.Short() {
		t.Skip("builds about 2,800 lines")
	}
	c, _ := fakeChat(t, t.TempDir(), 400)
	for i := range 400 {
		c.Log("user", fmt.Sprintf("%04d %s", i, strings.Repeat("u", 600)))
	}
	waitIdle(t, c)
	c.mu.Lock()
	inner := c.compact
	c.compact = nil // importing: nothing builds
	c.mu.Unlock()
	msgs := make([]Msg, 2000)
	for i := range msgs {
		msgs[i] = Msg{Kind: "opt", Text: fmt.Sprintf("imp %04d %s", i, strings.Repeat("n", 600))}
	}
	c.logMsgs(msgs)
	c.mu.Lock()
	c.compact = inner
	c.pump()
	c.mu.Unlock()
	waitFor(t, "the import built", func() bool { // fsyncs make it slow
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.busy) == 0 && len(c.ready0) == 0 && len(c.readyM) == 0
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if k := c.view.keys[0]; k.L > 6 {
		t.Fatalf("the oldest line, %s, covers more than 64 messages", k.name())
	}
}

// Lines whose compaction always fails hold up nothing: the lines after them
// get built, a call does not wait for them, and once they have failed while
// others got built, their source, cut and marked, stands in.
func TestFailingLines(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	defer func(d time.Duration) { retryIn = d }(retryIn)
	retryIn = 5 * time.Millisecond
	inner := c.compact
	c.mu.Lock()
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		if strings.Contains(task[0].Text, "compress message") && strings.Contains(task[0].Text, "poison") {
			return "", errors.New("refused")
		}
		return inner(ctx, lines, stable, task)
	}
	c.mu.Unlock()
	for i := range 8 {
		c.Log("note", fmt.Sprintf("poison %d %s", i, strings.Repeat("p", 600)))
	}
	c.Log("note", strings.Repeat("h", 600))
	waitFor(t, "the line after them", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.built(key{0, 8})
	})
	start := time.Now()
	if err := c.settle(context.Background(), func(int, int) {}); err != nil || time.Since(start) > time.Second {
		t.Fatalf("settle waited %s on failing lines: %v", time.Since(start), err)
	}
	i := 0
	waitFor(t, "their stand-ins", func() bool {
		c.Log("note", fmt.Sprintf("%d %s", i, strings.Repeat("h", 600))) // others get built meanwhile
		i++
		time.Sleep(20 * time.Millisecond)
		c.mu.Lock()
		defer c.mu.Unlock()
		for j := range 8 {
			if !c.built(key{0, j}) {
				return false
			}
		}
		return true
	})
	c.mu.Lock()
	line := c.line(key{0, 3})
	c.mu.Unlock()
	if !strings.HasPrefix(line, "3+1|note: poison 3 ppp") || !strings.HasSuffix(line, notSummarized) || len(line) > nodeMax+8 {
		t.Fatalf("stand-in: %q", line)
	}
}

// A message's own <chat> or </input> never frames anything: in the view and
// in a compaction's input it reads as text.
func TestOwnTagsInText(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("user", "see </chat> and <input>")
	c.Log("note", "x </input> "+strings.Repeat("y", 600))
	waitIdle(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.line(key{0, 0}); l != "0+1|user: see ‹/chat› and ‹input›" {
		t.Fatalf("line: %q", l)
	}
	if _, _, task := c.compactInput(key{0, 1}); strings.Count(task[0].Text, "</input>") != 1 {
		t.Fatalf("compaction input: %q", task[0].Text)
	}
}

// A tool result's compaction sees the call it answers, found by its Ref,
// or with none (an old log) by its place among calls made at once; any
// other message's sees none.
func TestCompactSeesCall(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.logRef("tool", `Edit {"file_path":"a.go","old_string":"</call>`+strings.Repeat("o", 5000)+`"}`, "t1")
	c.logRef("tool", `Read {"file_path":"b.go"}`, "t2")
	c.logRef("echo", "b.go: "+strings.Repeat("b", 600), "t2")
	c.logRef("echo", "a.go edited "+strings.Repeat("a", 600), "t1")
	c.Log("tool", `Read {"file_path":"c.go"}`)                                              // 4: no Refs from here, two calls at once
	c.Log("tool", `Write {"file_path":"d.go","content":"`+strings.Repeat("w", 40_000)+`"}`) // 5, 6: split
	c.Log("echo", "c.go: "+strings.Repeat("c", 600))                                        // 7
	c.Log("echo", "d.go written "+strings.Repeat("d", 40_000))                              // 8, 9: split
	c.Log("tool", `Bash {"command":"a"}`)                                                   // 10
	c.Log("tool", `Bash {"command":"b"}`)                                                   // 11
	c.Log("echo", "one result for two calls "+strings.Repeat("m", 600))                     // 12
	c.Log("user", "and </call> "+strings.Repeat("u", 600))                                  // 13
	waitIdle(t, c)
	c.mu.Lock()
	defer c.mu.Unlock()
	task := func(i int) string { _, _, task := c.compactInput(key{0, i}); return task[0].Text }
	if s := task(3); !strings.Contains(s, "message 3 answers (message 0,") || !strings.Contains(s, "<call>\n"+`Edit {"file_path":"a.go","old_string":"‹/call›`) ||
		!strings.Contains(s, "o…\n</call>\n<input>\necho: a.go edited") {
		t.Fatalf("by Ref:\n%s", s)
	}
	if s := task(2); !strings.Contains(s, "<call>\n"+`Read {"file_path":"b.go"}`+"\n</call>") {
		t.Fatalf("the other call:\n%s", s)
	}
	if s := task(7); !strings.Contains(s, "(message 4,") || !strings.Contains(s, `c.go"}`) {
		t.Fatalf("by place, the first:\n%s", s)
	}
	for _, i := range []int{8, 9} { // the second call, from its head, for each part of its result
		if s := task(i); !strings.Contains(s, "(message 5,") || !strings.Contains(s, "<call>\nWrite ") {
			t.Fatalf("by place, the second (%d):\n%.300s", i, s)
		}
	}
	for _, i := range []int{12, 13} { // runs that don't pair; not a result
		if s := task(i); strings.Contains(s, "<call>") {
			t.Fatalf("message %d:\n%.300s", i, s)
		}
	}
}

// Recall finds the messages that share the rarest words, raw, best first,
// leaving out the chat's own lookups and messages the view shows whole.
func TestRecall(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	pad := strings.Repeat("filler words here ", 40) // over nodeMax: summarized, so not shown whole
	for i := range 20 {
		c.Log("user", fmt.Sprintf("common talk %d about the project %s", i, pad))
	}
	c.Log("opt", "The staging host is 10.0.4.17, port 7443, the key in Zanzibar vault. "+pad) // 20
	c.Log("note", "Zanzibar again, nothing else")                                             // 21: whole in the view
	c.logRef("tool", `search {"text":"zanzibar"}`, "t1")                                      // 22
	c.logRef("echo", "2 messages hold \"zanzibar\": the staging host "+pad, "t1")             // 23
	c.Log("user", "Staging moved: the project's host is now 10.0.9.2. "+pad+" ZANZIBAR")      // 24
	c.Log("echo", strings.Repeat("a zanzibar staging key paste ", 400))                       // 25: all three words, but as long as it is, last
	c.Log("user", "where is the Zanzibar staging key?")                                       // 26: the question, after before
	waitIdle(t, c)
	// 24 holds zanzibar and staging too, but only 20 holds "key" as well.
	got := c.Recall("where is the Zanzibar staging key?", 26, 1)
	if !strings.Contains(got, "\n20+1|opt, ") || strings.Contains(got, "24+1|") || !strings.HasSuffix(got, "</recall>\n\n") {
		t.Fatalf("the best:\n%s", got)
	}
	got = c.Recall("where is the Zanzibar staging key?", 26, 8)
	if !strings.Contains(got, "\n20+1|") || !strings.Contains(got, "\n24+1|") || strings.Contains(got, "21+1|") || strings.Contains(got, "23+1|") || strings.Contains(got, "\n0+1|") {
		t.Fatalf("all:\n%s", got)
	}
	if i20, i24, i25 := strings.Index(got, "20+1|"), strings.Index(got, "24+1|"), strings.Index(got, "25+1|"); i20 > i24 || i24 > i25 {
		t.Fatalf("order:\n%s", got)
	}
	if got := c.Recall("is it so?", 26, 8); got != "" { // no word of three letters or more matches
		t.Fatalf("nothing: %q", got)
	}
	// Lowering can change byte lengths (K, a Kelvin sign, is 3 bytes; k one).
	if e := excerpt(strings.Repeat("K", 1000)+" Needle "+strings.Repeat("x", 1000), "needle", 1200); !strings.Contains(e, "Needle") {
		t.Fatalf("excerpt past a Kelvin sign: %d bytes, no Needle", len(e))
	}
	// A cut tail is marked, even where the start stepped back to a rune's.
	if e := excerpt("a"+strings.Repeat("é", 1000)+"needle"+"é", "needle", 101); !strings.HasSuffix(e, "…") {
		t.Fatalf("tail cut, not marked: %q", e)
	}
	if e := excerpt(strings.Repeat("a", 2000)+"Needle"+strings.Repeat("b", 2000), "needle", 100); !strings.HasPrefix(e, "…") ||
		!strings.Contains(e, "Needle") || !strings.HasSuffix(e, "…") || len(e) > 100+2*len("…") {
		t.Fatalf("excerpt: %q", e)
	}
}

// search finds messages by their words, newest first, a page at a time,
// naming the view's line that holds each, and skips the chat's own lookups.
func TestSearch(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	for i := range 30 {
		c.Log("user", fmt.Sprintf("message %d about the Gizmo", i))
	}
	c.logRef("tool", `search {"text":"gizmo"}`, "t1")
	c.logRef("echo", "30 messages hold \"gizmo\"", "t1")
	c.Log("note", "nothing here")
	waitIdle(t, c)
	got := c.Search("GIZMO", 0)
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], `30 messages hold "GIZMO"; the 20 newest before 33:`) || len(lines) != 22 ||
		!strings.HasPrefix(lines[1], "29+1|user: message 29 about the Gizmo (") || !strings.Contains(lines[1], "; in line ") ||
		lines[21] != `More: search("GIZMO", 10).` {
		t.Fatalf("search:\n%s", got)
	}
	if got := c.Search("gizmo", 10); !strings.HasPrefix(got, `10 messages hold "gizmo"; the 10 newest before 10:`) || strings.Contains(got, "More:") {
		t.Fatalf("page 2:\n%s", got)
	}
	if got := c.Search("wombat", 0); got != `No message before 33 holds "wombat".` {
		t.Fatal(got)
	}
	long := strings.Repeat("a", 500) + " needle " + strings.Repeat("b", 500)
	c.Log("note", long)
	if got := c.Search("needle", 0); !strings.Contains(got, "|note: …"+strings.Repeat("a", 99)+" needle "+strings.Repeat("b", 149)+"…") {
		t.Fatalf("snippet:\n%s", got)
	}
	// Not held as written: the messages that share its words.
	if got := c.Search("haystack needle", 0); !strings.HasPrefix(got, `No message before 34 holds "haystack needle". These share its words`) ||
		!strings.Contains(got, "\n33+1|note, ") {
		t.Fatalf("by words:\n%s", got)
	}
}

// An import is a side of its own: the chat's lines build first, a call
// waits for the chat's alone, and a merge far ahead of its side's messages
// waits for them, its context within the compaction view's cap.
func TestImportSide(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 400)
	inner := c.compact
	var mu sync.Mutex
	var order []string
	most := 0
	c.mu.Lock()
	c.compact = nil // importing: nothing builds
	c.mu.Unlock()
	var imp []Msg
	for i := range 300 {
		text := fmt.Sprintf("short %d", i) // free, and their merges far ahead
		if i%20 == 0 {
			text = fmt.Sprintf("long %d %s", i, strings.Repeat("l", 600))
		}
		imp = append(imp, Msg{Kind: kindAI, Text: text, Imported: true})
	}
	c.logMsgs(imp)
	c.Log("user", "mine "+strings.Repeat("m", 600))
	c.mu.Lock()
	c.jobs = 2
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		mu.Lock()
		order = append(order, strings.SplitN(task[0].Text, "\n", 2)[0])
		most = max(most, lo.SumBy(lines, func(l string) int { return len(l) + 1 }))
		mu.Unlock()
		if lines := lines[:stable]; slices.ContainsFunc(lines, func(l string) bool { return strings.HasSuffix(l, unbuilt) }) {
			t.Errorf("a placeholder in the cached part of a context: %q", lines)
		}
		return inner(ctx, lines, stable, task)
	}
	c.pump()
	c.mu.Unlock()
	start := time.Now()
	if err := c.settle(context.Background(), func(int, int) {}); err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("settle: %v after %s", err, time.Since(start))
	}
	c.mu.Lock()
	mine := c.built(key{0, 300})
	c.mu.Unlock()
	if !mine {
		t.Fatal("settle went on before the chat's own message was built")
	}
	waitIdle(t, c)
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(order[:2], func(s string) bool { return strings.Contains(s, "compress message 300 ") }) { // the first two start at once
		t.Fatalf("the chat's message did not go first: %q", order[:3])
	}
	if most > cviewHi {
		t.Fatalf("a compaction saw %d bytes of context", most)
	}
}

// A line that is only "ok" (a compaction that answered the view's first
// message) is built again at load, and so is every line built on it, their
// old text standing in until then; a reload after that redoes nothing.
func TestRedoAcks(t *testing.T) {
	dir := t.TempDir()
	c, _ := fakeChat(t, dir, 300)
	for range 4 {
		c.Log("echo", strings.Repeat("x", 600))
	}
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	waitIdle(t, c)
	if !c.built(key{2, 0}) {
		t.Fatal("tree not built")
	}
	copyDir := t.TempDir()
	if err := os.CopyFS(copyDir, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(copyDir, "tree", "*.jsonl"))
	if err := appendRaw(files[len(files)-1], []byte(`{"l":0,"i":1,"text":"ok","size":2}`+"\n")); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(copyDir, "lock"))
	r, err := openChat(copyDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[key]bool{{0, 1}: true, {1, 0}: true, {2, 0}: true}
	if !maps.Equal(r.redo, want) {
		t.Fatalf("redo = %v, want %v", r.redo, want)
	}
	if r.text(key{2, 0}) != strings.Repeat("s", 300) {
		t.Fatal("a line to redo lost its old text before it was built again")
	}
	// Lines to redo hold no call back, and a new message goes before them.
	start := time.Now()
	if err := r.settle(context.Background(), func(int, int) {}); err != nil || time.Since(start) > time.Second || r.unsummarized() != 0 {
		t.Fatalf("settle waited for lines to redo: %v after %s", err, time.Since(start))
	}
	r.Log("user", strings.Repeat("n", 600))
	var mu sync.Mutex
	var order []string
	r.mu.Lock()
	r.jobs = 1
	r.compact = func(_ context.Context, _ []string, _ int, task []block) (string, error) {
		mu.Lock()
		order = append(order, strings.Join(strings.Fields(task[0].Text)[1:5], " "))
		mu.Unlock()
		return strings.Repeat("f", 300), nil
	}
	r.pump()
	r.mu.Unlock()
	waitIdle(t, r)
	if mu.Lock(); !slices.Equal(order, []string{"compress message 4 into", "compress message 1 into", "merge lines 0+1 and", "merge lines 0+2 and"}) {
		t.Fatalf("compactions: %q", order)
	}
	mu.Unlock()
	if r.tree[key{0, 4}] != strings.Repeat("f", 300) {
		t.Fatal("the new message was not built")
	}
	if want := lo.SumBy(r.cview.keys, func(k key) int { return cost(r, k) }); r.cview.bytes != want {
		t.Fatalf("compaction view size %d after redo, want %d", r.cview.bytes, want)
	}
	f := strings.Repeat("f", 300)
	if len(r.redo) != 0 || r.tree[key{0, 1}] != f || r.tree[key{1, 0}] != f || r.tree[key{2, 0}] != f {
		t.Fatalf("not redone: redo %v, %.3q %.3q %.3q", r.redo, r.tree[key{0, 1}], r.tree[key{1, 0}], r.tree[key{2, 0}])
	}
	if r.tree[key{0, 0}] != strings.Repeat("s", 300) || r.tree[key{1, 1}] != strings.Repeat("s", 300) {
		t.Fatal("a sound line was built again")
	}
	if want := lo.SumBy(r.view.keys, func(k key) int { return cost(r, k) }); r.view.bytes != want {
		t.Fatalf("view size %d after redo, want %d", r.view.bytes, want)
	}
	r.mu.Lock()
	r.compact = nil
	r.mu.Unlock()
	again := t.TempDir()
	if err := os.CopyFS(again, os.DirFS(copyDir)); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(again, "lock"))
	r2, err := openChat(again)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.redo) != 0 || r2.tree[key{2, 0}] != f {
		t.Fatalf("reload redoes again: %v", r2.redo)
	}
}

// A compaction past the compactions' view sees, in its tail, the messages
// from that view's end on once each, even where the view holds a merge
// that began inside the compactions' view.
func TestCompactTailInMerge(t *testing.T) {
	c, err := openChat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		c.Log("user", strings.Repeat("u", 600)) // none free: nothing builds without a compactor
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range []key{{0, 0}, {0, 1}, {0, 2}, {0, 3}, {1, 0}, {1, 1}, {2, 0}} {
		c.tree[k] = "line " + k.name()
	}
	c.view.keys = []key{{2, 0}, {0, 4}, {0, 5}, {0, 6}, {0, 7}}
	c.cview.keys = []key{{0, 0}, {0, 1}}
	var names []string
	for _, l := range c.compactTail(key{0, 6}, 2, 0) {
		names = append(names, strings.SplitN(l, "|", 2)[0])
	}
	if want := []string{"2+2", "4+1", "5+1"}; !slices.Equal(names, want) {
		t.Fatalf("tail %q, want %q", names, want)
	}
	if got := cover(5, 16); !slices.Equal(got, []key{{0, 5}, {1, 3}, {3, 1}}) {
		t.Fatalf("cover(5, 16) = %v", got)
	}
}

// The compactions' view batches on its own as it passes cviewHi: messages
// built one after another past 32 KB still see the chat up to their own.
// It froze at its cap until the chat's view batched, and with no budget
// left for a tail, message 300 saw context ending at message 78.
func TestCviewBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 160 lines, synced one by one")
	}
	c, _ := fakeChat(t, t.TempDir(), 400)
	inner := c.compact
	c.mu.Lock()
	c.compact = nil
	c.mu.Unlock()
	for range 160 {
		c.Log("user", strings.Repeat("u", 600))
	}
	var mu sync.Mutex
	ends := map[int]int{} // message compressed: where its context ends
	c.mu.Lock()
	c.jobs = 1
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		if m := regexp.MustCompile(`compress message (\d+) `).FindStringSubmatch(task[0].Text); m != nil && len(lines) > 0 {
			i, _ := strconv.Atoi(m[1])
			h := lineHead.FindStringSubmatch(lines[len(lines)-1])
			id, _ := strconv.Atoi(h[1])
			n, _ := strconv.Atoi(h[2])
			mu.Lock()
			ends[i] = id + n
			mu.Unlock()
		}
		return inner(ctx, lines, stable, task)
	}
	c.pump()
	c.mu.Unlock()
	waitIdle(t, c)
	mu.Lock()
	defer mu.Unlock()
	for _, i := range []int{100, 150} {
		if ends[i] != i {
			t.Errorf("compressing message %d saw context ending at message %d", i, ends[i])
		}
	}
}

// A line to redo before the compactions' view's end sees, past its
// context, the view's lines up to it, split where they reach past it.
// Its context was only the compactions' view's lines ending before it.
func TestCompactTailRedo(t *testing.T) {
	c, err := openChat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for range 8 {
		c.Log("user", strings.Repeat("u", 600))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for l := range 3 {
		for i := range 8 >> l {
			c.tree[key{l, i}] = "line " + key{l, i}.name()
		}
	}
	c.view.keys = []key{{2, 0}, {2, 1}}
	c.cview.keys = []key{{2, 0}, {2, 1}}
	for k, want := range map[key][]string{{0, 5}: {"4+1"}, {0, 2}: {"0+2"}, {0, 6}: {"4+2"}} {
		c.redo[k] = true
		ctx, _, _ := c.compactInput(k)
		var names []string
		for _, l := range ctx {
			names = append(names, strings.SplitN(l, "|", 2)[0])
		}
		if want = append([]string{"0+4"}, want...); k.start() < 4 {
			want = want[1:]
		}
		if !slices.Equal(names, want) {
			t.Errorf("redo %s: context %q, want %q", k.name(), names, want)
		}
		delete(c.redo, k)
	}
}

// For a line not to redo, the tail is as before lines to redo got one:
// the view's lines from the compactions' view's end on.
func TestCompactTailUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("builds 200 lines, synced one by one")
	}
	c, _ := fakeChat(t, t.TempDir(), 400)
	inner := c.compact
	c.mu.Lock()
	c.compact = nil
	c.mu.Unlock()
	var imp []Msg
	for i := range 150 {
		imp = append(imp, Msg{Kind: kindAI, Text: strings.Repeat("i", lo.Ternary(i%3 == 0, 600, 100)), Imported: true})
	}
	c.logMsgs(imp)
	for range 50 {
		c.Log("user", strings.Repeat("u", 600))
	}
	old := func(k key, size int) []string { // compactTail of f46c515
		from := c.cview.end()
		if from >= until(k) {
			return nil
		}
		at := func(s int) int {
			j, _ := slices.BinarySearchFunc(c.view.keys, s, func(v key, s int) int { return cmp.Compare(v.start(), s) })
			return j
		}
		j := at(from+1) - 1
		keys := slices.Concat(cover(from, c.view.keys[j].end()), c.view.keys[j+1:at(until(k))])
		var tail []string
		for _, v := range slices.Backward(keys) {
			l := c.callLine(v)
			if size += len(l) + 1; size > cviewHi {
				break
			}
			tail = append(tail, l)
		}
		slices.Reverse(tail)
		return tail
	}
	checked, tails := 0, 0
	c.mu.Lock()
	c.compact = func(ctx context.Context, lines []string, stable int, task []block) (string, error) {
		c.mu.Lock()
		for _, k := range slices.Concat(lo.Map(c.ready0, func(i int, _ int) key { return key{0, i} }), c.readyM) {
			keys := c.compactContext(k)
			from, size := 0, lo.SumBy(c.lines(keys), func(l string) int { return len(l) + 1 })
			if len(keys) > 0 {
				from = keys[len(keys)-1].end()
			}
			got, want := c.compactTail(k, from, size), old(k, size)
			if !slices.Equal(got, want) {
				t.Errorf("%s: tail of %d lines, before %d", k.name(), len(got), len(want))
			}
			checked, tails = checked+1, tails+min(len(got), 1)
		}
		c.mu.Unlock()
		return inner(ctx, lines, stable, task)
	}
	c.pump()
	c.mu.Unlock()
	waitIdle(t, c)
	if checked < 1000 || tails < 100 {
		t.Fatalf("checked %d tails, %d of them not empty", checked, tails)
	}
}

// -redo marks a message's line that holds what its message lacks, and the
// lines built on it, and keeps the marks in the tree's log: a line whose
// message says it too stays.
func TestMarkMatching(t *testing.T) {
	dir := t.TempDir()
	c, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		c.Log("tool", fmt.Sprint("commit ", i, map[bool]string{true: " (should be Brand X)"}[i == 3], strings.Repeat(" x", 400))) // over 512: a model's line
	}
	c.mu.Lock()
	for l := range 3 {
		for i := range 4 >> l {
			c.setNode(key{l, i}, "line "+key{l, i}.name())
		}
	}
	c.setNode(key{0, 1}, "commit 1, should be Brand X")
	c.setNode(key{0, 3}, "commit 3, should be Brand X")
	c.flushNodes()
	c.mu.Unlock()
	if n := c.markMatching(regexp.MustCompile(`Brand X`)); n != 3 {
		t.Fatalf("marked %d, want 3: 0+1, 0+2, 0+4", n)
	}
	c.lock.Close() // as at a restart
	again, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.SortedFunc(maps.Keys(again.redo), func(a, b key) int { return cmp.Compare(a.L*100+a.I, b.L*100+b.I) })
	if want := []key{{0, 1}, {1, 0}, {2, 0}}; !slices.Equal(got, want) {
		t.Fatalf("after a restart, to redo: %v, want %v", got, want)
	}
}
