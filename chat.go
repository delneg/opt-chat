package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/samber/lo"
)

const (
	nodeMax = 512     // target bytes of one summary line
	viewLo  = 64_000  // the chat's view drops back to this...
	viewHi  = 128_000 // ...once it passes this
	cviewLo = 16_000  // same sawtooth for the compactions' view
	cviewHi = 32_000
	jobs    = 8      // compactions running at once
	clipMax = 30_000 // characters: a tool output's head and tail, and the most in one message
	retryIn = 10 * time.Second
	// A stuck compaction must not hold a job slot forever.
	compactTimeout = 5 * time.Minute
	unbuilt        = "(not summarized yet: zoom it)"
)

// key names tree node (l, i): the 2^l messages from i·2^l on.
type key struct{ L, I int }

func (k key) start() int       { return k.I << k.L }
func (k key) end() int         { return (k.I + 1) << k.L }
func (k key) name() string     { return fmt.Sprintf("%d+%d", k.start(), 1<<k.L) }
func (k key) parent() key      { return key{k.L + 1, k.I / 2} }
func (k key) kids() (a, b key) { return key{k.L - 1, 2 * k.I}, key{k.L - 1, 2*k.I + 1} }

type Msg struct {
	I      int       `json:"i"`
	Kind   string    `json:"kind"`
	Text   string    `json:"text"`
	Size   int       `json:"size"`
	Date   time.Time `json:"date"`
	Images []string  `json:"images,omitzero"` // files/<hash>.<ext>, named by content
}

type Node struct {
	L    int    `json:"l"`
	I    int    `json:"i"`
	Text string `json:"text"`
	Size int    `json:"size"`
}

// Chat is the log, the tree and both views. All fields are guarded by mu.
type Chat struct {
	dir     string
	lock    *os.File // held for the life of the process: if collected, its finalizer would unlock
	mu      sync.Mutex
	msgs    []Msg
	tree    map[key]string
	view    view         // what turns see
	cview   view         // what compactions see
	queued  map[key]bool // waiting in ready0/readyM or running
	busy    map[key]bool // running
	cooling map[key]bool // failed: tried again at the next message, or after retryIn
	failed  map[key]bool // already reported
	ready0  []int        // messages whose line needs a model call, in order
	readyM  []key        // merges whose line needs a model call
	changed chan struct{}
	// compact builds one line with a model; set before the first pump, nil
	// again at quit.
	compact func(ctx context.Context, context []string, task []block) (string, error)
}

func openChat(dir string) (*Chat, error) {
	for _, d := range []string{"main", "tree", "files"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// The OS frees the lock when the process dies: no stale locks.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another opt-chat owns %s", dir)
	}
	c := &Chat{
		dir: dir, lock: lock, tree: map[key]string{}, queued: map[key]bool{}, busy: map[key]bool{},
		cooling: map[key]bool{}, failed: map[key]bool{}, changed: make(chan struct{}),
		view: view{lo: viewLo, hi: viewHi}, cview: view{lo: cviewLo, hi: cviewHi},
	}
	err = load(filepath.Join(dir, "main"), func(m Msg) {
		if m.I != len(c.msgs) {
			warn("main: message %d out of order, skipped", m.I)
			return
		}
		c.msgs = append(c.msgs, m)
	})
	if err != nil {
		return nil, err
	}
	if err := load(filepath.Join(dir, "tree"), func(n Node) { c.tree[key{n.L, n.I}] = n.Text }); err != nil {
		return nil, err
	}
	// The view is kept, never rebuilt: a rebuilt view differs and kills every cache entry.
	if b, err := os.ReadFile(filepath.Join(dir, "view.json")); err == nil {
		var pairs [][2]int
		if err := json.Unmarshal(b, &pairs); err != nil {
			return nil, fmt.Errorf("view.json: %w", err)
		}
		c.view.keys = lo.Map(pairs, func(p [2]int, _ int) key { return key{p[0], p[1]} })
	}
	end := 0
	if n := len(c.view.keys); n > 0 {
		end = c.view.keys[n-1].end()
	}
	for i := end; i < len(c.msgs); i++ { // messages logged after the last save
		c.view.push(key{0, i}, c)
	}
	c.resetCview()
	c.saveView()
	c.enqueueMissing()
	return c, nil
}

// load reads every day file of dir in order, skipping torn lines.
func load[T any](dir string, each func(T)) error {
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return err
	}
	slices.Sort(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if len(b) > 0 && b[len(b)-1] != '\n' { // a crash mid-write: start the next write on its own line
			if err := appendRaw(f, []byte("\n")); err != nil {
				return err
			}
		}
		for line := range bytes.SplitSeq(b, []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var v T
			if err := json.Unmarshal(line, &v); err != nil {
				warn("%s: torn line skipped: %v", filepath.Base(f), err)
				continue
			}
			each(v)
		}
	}
	return nil
}

func appendRaw(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// appendDay appends v as one line to sub/<today>.jsonl and fsyncs it.
func (c *Chat) appendDay(sub string, v any) {
	b, err := json.Marshal(v)
	if err == nil {
		err = appendRaw(filepath.Join(c.dir, sub, time.Now().Format(time.DateOnly)+".jsonl"), append(b, '\n'))
	}
	if err != nil {
		// ponytail: a failed write is fatal; the log is history and must not have holes.
		fatal("write %s: %v", sub, err)
	}
}

// Log appends one message, splitting text too long for one. Its images
// (names from saveImage) go with the first part.
func (c *Chat) Log(kind, text string, images ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for j, part := range splitText(text, clipMax) {
		i := len(c.msgs)
		m := Msg{I: i, Kind: kind, Text: part, Size: len(kind) + 2 + len(part), Date: time.Now()}
		if j == 0 {
			m.Images = images
		}
		c.appendDay("main", m)
		c.msgs = append(c.msgs, m)
		c.need(key{0, i})
		if c.view.push(key{0, i}, c) {
			c.resetCview()
		} else {
			c.cview.push(key{0, i}, c)
		}
		c.saveView()
	}
	clear(c.cooling) // a failed compaction is tried again at the next message
	c.pump()
	c.notify()
}

// saveImage stores an image in files/, named by its content, and returns that name.
func (c *Chat) saveImage(b []byte) (string, error) {
	ext, ok := strings.CutPrefix(http.DetectContentType(b), "image/")
	if !ok || !slices.Contains([]string{"png", "jpeg", "gif", "webp"}, ext) {
		return "", fmt.Errorf("not a PNG, JPEG, GIF or WebP image")
	}
	sum := sha256.Sum256(b)
	name := filepath.Join("files", hex.EncodeToString(sum[:16])+"."+ext)
	if _, err := os.Stat(filepath.Join(c.dir, name)); err == nil {
		return name, nil
	}
	f, err := os.CreateTemp(filepath.Join(c.dir, "files"), "tmp-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return name, os.Rename(f.Name(), filepath.Join(c.dir, name))
}

// imageBlocks loads saved images as content blocks for a call.
func (c *Chat) imageBlocks(names []string) []block {
	return lo.Map(names, func(name string, _ int) block {
		b, err := os.ReadFile(filepath.Join(c.dir, name))
		if err != nil {
			return textBlock("[image lost: " + name + "]")
		}
		mt := "image/" + strings.TrimPrefix(filepath.Ext(name), ".")
		return block{Type: "image", Source: &source{Type: "base64", MediaType: mt, Data: base64.StdEncoding.EncodeToString(b)}}
	})
}

func (c *Chat) saveView() {
	b, _ := json.Marshal(lo.Map(c.view.keys, func(k key, _ int) [2]int { return [2]int{k.L, k.I} }))
	tmp := filepath.Join(c.dir, "view.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		fatal("view.json: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, "view.json")); err != nil {
		fatal("view.json: %v", err)
	}
}

func (c *Chat) resetCview() {
	c.cview.keys = slices.Clone(c.view.keys)
	c.cview.shrinking = true
	c.cview.fit(c)
}

func (c *Chat) built(k key) bool { _, ok := c.tree[k]; return ok }

func (c *Chat) text(k key) string {
	if t, ok := c.tree[k]; ok {
		return t
	}
	return unbuilt
}

func (c *Chat) line(k key) string {
	return k.name() + "|" + strings.ReplaceAll(c.text(k), "\n", " ")
}

func (c *Chat) lines(keys []key) []string {
	return lo.Map(keys, func(k key, _ int) string { return c.line(k) })
}

// freeText is the node's text when its source fits in one line, with no model call.
func (c *Chat) freeText(k key) (string, bool) {
	if k.L == 0 {
		m := c.msgs[k.I]
		return m.Kind + ": " + m.Text, m.Size <= nodeMax && len(m.Images) == 0 // an image takes a model to put in words
	}
	a, b := k.kids()
	s := c.tree[a] + "\n" + c.tree[b]
	return s, len(s) <= nodeMax
}

// need queues node k, or builds it at once when it is free.
func (c *Chat) need(k key) {
	if c.built(k) || c.queued[k] {
		return
	}
	if t, ok := c.freeText(k); ok {
		c.setNode(k, t)
		return
	}
	c.queued[k] = true
	if k.L == 0 {
		c.ready0 = append(c.ready0, k.I)
	} else {
		c.readyM = append(c.readyM, k)
	}
}

func (c *Chat) setNode(k key, text string) {
	c.tree[k] = text
	c.appendDay("tree", Node{L: k.L, I: k.I, Text: text, Size: len(text)})
	if c.queued[k] {
		delete(c.queued, k)
		if k.L == 0 {
			c.ready0 = slices.DeleteFunc(c.ready0, func(i int) bool { return i == k.I })
		} else {
			c.readyM = slices.DeleteFunc(c.readyM, func(m key) bool { return m == k })
		}
	}
	if c.built(key{k.L, k.I ^ 1}) {
		c.need(k.parent())
	}
}

// enqueueMissing finds the work a previous run left undone. Run once, at start.
func (c *Chat) enqueueMissing() {
	T := len(c.msgs)
	for i := range T {
		c.need(key{0, i})
	}
	for l := 0; 2<<l <= T; l++ {
		for p := range T >> (l + 1) {
			if k := (key{l + 1, p}); c.built(key{l, 2 * p}) && c.built(key{l, 2*p + 1}) {
				c.need(k)
			}
		}
	}
}

// next picks the next node to build: messages first, at most 8 deep, then merges.
func (c *Chat) next() (key, bool) {
	free := func(k key) bool { return !c.busy[k] && !c.cooling[k] }
	for _, i := range c.ready0[:min(jobs, len(c.ready0))] {
		if k := (key{0, i}); free(k) {
			return k, true
		}
	}
	return lo.Find(c.readyM, free)
}

func (c *Chat) pump() {
	for c.compact != nil && len(c.busy) < jobs { // nil: importing, the next run builds
		k, ok := c.next()
		if !ok {
			return
		}
		c.busy[k] = true
		ctxLines, task := c.compactInput(k)
		go c.build(k, c.compact, ctxLines, task)
	}
}

func (c *Chat) build(k key, compact func(context.Context, []string, []block) (string, error), ctxLines []string, task []block) {
	ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
	text, err := compact(ctx, ctxLines, task)
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.busy, k)
	if err != nil && c.compact == nil {
		return // quitting: the next run builds it
	}
	if err != nil {
		if !c.failed[k] {
			c.failed[k] = true
			warn("compaction %s failed, trying again at the next message or in %s: %v", k.name(), retryIn, err)
		}
		c.cooling[k] = true
		// ponytail: also on a timer, so a turn waiting on this line never waits for a message that won't come.
		time.AfterFunc(retryIn, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			delete(c.cooling, k)
			c.pump()
		})
		return
	}
	delete(c.failed, k)
	c.setNode(k, text)
	c.pump()
	c.notify()
}

var ruler = strings.Repeat("-", nodeMax)

// compactInput is node k's context (the compaction view up to the node,
// stopping at the first unbuilt line) and its task.
func (c *Chat) compactInput(k key) ([]string, []block) {
	until := k.end()
	if k.L == 0 {
		until = k.I
	}
	var keys []key
	for _, v := range c.cview.keys {
		if v.end() > until || !c.built(v) {
			break
		}
		keys = append(keys, v)
	}
	if k.L == 0 {
		m := c.msgs[k.I]
		in := fmt.Sprintf("Compaction: compress message %d into one line of at most 512 bytes\n"+
			"(about 70 words), the length of this ruler:\n%s\n<input>\n%s: %s\n", k.I, ruler, m.Kind, m.Text)
		if len(m.Images) == 0 {
			return c.lines(keys), []block{textBlock(in + "</input>")}
		}
		return c.lines(keys), slices.Concat([]block{textBlock(in)}, c.imageBlocks(m.Images), []block{textBlock("</input>")})
	}
	a, b := k.kids()
	return c.lines(keys), []block{textBlock(fmt.Sprintf("Compaction: merge lines %s and %s, adjacent, into one line of at most\n"+
		"512 bytes (about 70 words), the length of this ruler:\n%s\n"+
		"<chat> may hold their messages, %d to %d, in more detail: take details\nof them from there too.\n"+
		"<input>\n%s\n%s\n</input>",
		a.name(), b.name(), ruler, k.start(), k.end()-1, c.line(a), c.line(b)))}
}

func (c *Chat) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// settle waits until every message is summarized, so no call sees a placeholder.
func (c *Chat) settle(ctx context.Context, waiting func(n int)) error {
	told := false
	for {
		c.mu.Lock()
		n, ch := len(c.ready0), c.changed
		c.mu.Unlock()
		if n == 0 {
			return nil
		}
		if !told {
			waiting(n)
			told = true
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Zoom opens line id+n into its two halves; n = 1 gives the message whole,
// with its images.
func (c *Chat) Zoom(id, n int) (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 1 || n&(n-1) != 0 || id < 0 || id%n != 0 || id+n > len(c.msgs) {
		return fmt.Sprintf("No line %d+%d.", id, n), nil
	}
	if n == 1 {
		m := c.msgs[id]
		return fmt.Sprintf("%d+1|%s: %s", id, m.Kind, m.Text), m.Images
	}
	a, b := key{bits.TrailingZeros(uint(n)), id / n}.kids()
	return c.line(a) + "\n" + c.line(b), nil
}

func (c *Chat) Date(id int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id < 0 || id >= len(c.msgs) {
		return fmt.Sprintf("No message %d.", id)
	}
	return c.msgs[id].Date.Local().Format("Mon 2006-01-02 15:04:05 MST")
}

// splitText cuts s into pieces of at most n characters.
func splitText(s string, n int) []string {
	var out []string
	for utf8.RuneCountInString(s) > n {
		i := runeOffset(s, n)
		out = append(out, s[:i])
		s = s[i:]
	}
	return append(out, s)
}

// runeOffset is the byte offset of s's nth character.
func runeOffset(s string, n int) int {
	for i := range s {
		if n == 0 {
			return i
		}
		n--
	}
	return len(s)
}

// head is s's first n bytes at most, never cutting a character.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// clip keeps the head and tail of a tool output, at most n characters in all.
func clip(s string, n int) string {
	total := utf8.RuneCountInString(s)
	if total <= n {
		return s
	}
	half := max(0, n-utf8.RuneCountInString(fmt.Sprintf("\n[... %d characters cut ...]\n", total))) / 2
	h, t := runeOffset(s, half), runeOffset(s, total-half)
	return fmt.Sprintf("%s\n[... %d characters cut ...]\n%s", s[:h], total-2*half, s[t:])
}
