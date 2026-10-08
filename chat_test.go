package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json/v2"

	"github.com/samber/lo"
)

var lineHead = regexp.MustCompile(`(?m)^(\d+)\+(\d+)\|`)

// fakeChat opens a chat whose compactor checks what every compaction sees
// (built lines only, none past the node) and answers with a size-byte line.
func fakeChat(t *testing.T, dir string, size int) (*Chat, *atomic.Int64) {
	t.Helper()
	c, err := openChat(dir)
	if err != nil {
		t.Fatal(err)
	}
	calls := new(atomic.Int64)
	msgRe := regexp.MustCompile(`compress message (\d+) `)
	mergeRe := regexp.MustCompile(`merge lines (\S+) and (\S+), adjacent(?s:.*)messages, (\d+) to (\d+), in more detail(?s:.*)<input>\n(.*)\n(.*)\n</input>$`)
	c.compact = func(_ context.Context, lines []string, blocks []block) (string, error) {
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
			if strings.Contains(l, unbuilt) {
				t.Errorf("compaction saw a placeholder: %q", l)
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
	deadline := time.After(10 * time.Second)
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
		cur, size, csize := c.view.keys, c.view.size(c), c.cview.size(c)
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
	c.compact = func(ctx context.Context, lines []string, task []block) (string, error) {
		if strings.Contains(task[0].Text, "compress message 0 ") {
			seen = task
		}
		return inner(ctx, lines, task)
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
	if b := c.imageBlocks([]string{"files/gone.png"}); b[0].Type != "text" {
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

// A failed compaction is tried again at the next message, not only on the timer.
func TestRetryAtNextMessage(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	inner := c.compact
	var fails atomic.Int64
	fails.Store(1)
	c.compact = func(ctx context.Context, lines []string, task []block) (string, error) {
		if fails.Add(-1) >= 0 {
			return "", fmt.Errorf("down")
		}
		return inner(ctx, lines, task)
	}
	c.Log("user", strings.Repeat("a", 600))
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		c.mu.Lock()
		cooling := c.cooling[key{0, 0}]
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
	release := a.writeOnce("p")
	other := a.writeOnce("q") // another prefix never waits
	other()
	var waited atomic.Int64
	done := make(chan struct{})
	for range 3 {
		go func() {
			a.writeOnce("p")() // waits for the writer, then goes on with the rest at once
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
	a.writeOnce("p")() // free again
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
	compact := func(ctx context.Context, lines []string, task []block) (string, error) {
		if n := int64(lo.SumBy(lines, func(l string) int { return len(l) + 1 })); n > most.Load() {
			most.Store(n)
		}
		return inner(ctx, lines, task)
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
	size, csize := c.view.size(c), c.cview.size(c)
	c.mu.Unlock()
	if size > viewHi || csize > cviewHi {
		t.Fatalf("after the backlog: view %d bytes, compaction view %d", size, csize)
	}
	if m := most.Load(); m > cviewHi+jobs*(nodeMax+16) {
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
	if !tiles([][2]int{{1, 0}, {0, 2}}, 3) || tiles([][2]int{{0, 0}, {0, 2}}, 3) || tiles([][2]int{{2, 0}}, 3) || tiles([][2]int{{99, 0}}, 3) {
		t.Fatal("tiles")
	}
}
