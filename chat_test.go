package main

import (
	"context"
	"fmt"
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
	mergeRe := regexp.MustCompile(`messages, (\d+) to (\d+), in more detail`)
	c.compact = func(_ context.Context, lines []string, task string) (string, error) {
		calls.Add(1)
		limit := -1
		if m := msgRe.FindStringSubmatch(task); m != nil {
			limit, _ = strconv.Atoi(m[1]) // lines must end before the message
		} else if m := mergeRe.FindStringSubmatch(task); m != nil {
			limit, _ = strconv.Atoi(m[2])
			limit++ // lines may cover the merge itself
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
	c.Log("echo", strings.Repeat("x", 600))          // over 512: needs a model call
	c.Log("user", strings.Repeat("é", clipMax/2+10)) // over clipMax: logged as two messages
	c.mu.Lock()
	c.pump()
	c.mu.Unlock()
	if err := c.settle(context.Background(), func(int) {}); err != nil {
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
	if calls.Load() == 0 {
		t.Fatal("no model call for a long message")
	}
	if got := c.Zoom(0, 2); got != "0+1|user: hi\n1+1|opt: hello" {
		t.Fatalf("zoom(0, 2) = %q", got)
	}
	if got := c.Zoom(2, 1); got != "2+1|echo: "+strings.Repeat("x", 600) {
		t.Fatalf("zoom(2, 1) = %.40q", got)
	}
	for _, bad := range [][2]int{{1, 2}, {0, 3}, {4, 8}, {-1, 1}, {5, 1}} {
		if got := c.Zoom(bad[0], bad[1]); !strings.HasPrefix(got, "No line") {
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
	first, second := viewMessages(lines[:30], "hi")
	if first != nil || second[0] != "<chat>\n"+strings.Join(lines[:30], "\n")+"\n</chat>" || second[1] != "hi" {
		t.Fatalf("short view: %q / %q", first, second)
	}
	a, _ := viewMessages(lines[:130], "x")
	b, rest := viewMessages(lines[:150], "y")
	if !slices.Equal(a, b) {
		t.Fatal("the first message changed while the view grew within a checkpoint")
	}
	if !strings.HasSuffix(b[0], viewGoesOn) || !strings.Contains(b[0], lines[127]+"\n</chat>") {
		t.Fatalf("first message: %.60q ... %.60q", b[0], b[0][len(b[0])-60:])
	}
	if rest[0] != "<chat>\n"+strings.Join(lines[128:], "\n")+"\n</chat>" || rest[1] != "y" {
		t.Fatalf("second message: %q", rest)
	}
	if e, _ := viewMessages(nil, "z"); e != nil {
		t.Fatal("empty view got a first message")
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
