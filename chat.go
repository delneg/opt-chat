package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	jobs    = 8                // compactions running at once
	clipMax = 30_000           // characters: a tool output's head and tail, and the most in one message
	waitMax = 10 * time.Minute // a failed compaction's longest wait
	giveUp  = 4                // failures while other lines got built: the line is its source, cut
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
	Images []string  `json:"images,omitzero"` // files/<hash>.<ext>, named by content: images and PDFs, which go to the model
	Ref    string    `json:"ref,omitzero"`    // a tool call's id, on it and on its echo
	Part   int       `json:"part,omitzero"`   // of a text too long for one message: 1 on, the parts after its first
}

type Node struct {
	L    int    `json:"l"`
	I    int    `json:"i"`
	Text string `json:"text"`
	Size int    `json:"size"`
}

// failure is a line's compactions that failed: n in a row, the strikes among
// them that came after other lines were built (so not an outage or a usage
// limit, which fail every line), and when it is tried again.
type failure struct {
	n, strikes, at int // at: c.written at the last one
	until          time.Time
}

// Chat is the log, the tree and both views. All fields are guarded by mu.
type Chat struct {
	dir     string
	lock    *os.File // held for the life of the process: if collected, its finalizer would unlock
	mu      sync.Mutex
	msgs    []Msg
	asides  []Aside // side questions: never in msgs, see btw.go
	tree    map[key]string
	view    view             // what turns see
	cview   view             // what compactions see
	queued  map[key]bool     // waiting in ready0/readyM or running
	busy    map[key]bool     // running
	failing map[key]*failure // lines whose compaction failed, until built
	ready0  []int            // messages whose line needs a model call, in order
	readyM  []key            // merges whose line needs a model call
	written int              // lines a model has built in this run
	changed chan struct{}
	hub     *hub // where what happens is shown; nil: nowhere
	// compact builds one line with a model; set before the first pump, nil
	// again at quit.
	compact func(ctx context.Context, context []string, task []block) (string, error)
}

func openChat(dir string) (*Chat, error) {
	for _, d := range []string{"main", "tree", "files", "btw", "usage"} {
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
		failing: map[key]*failure{}, changed: make(chan struct{}),
		view: view{lo: viewLo, hi: viewHi}, cview: view{lo: cviewLo, hi: cviewHi},
	}
	// By id, not by file: a day file is named by the clock, which can go back.
	var msgs []Msg
	if err := load(filepath.Join(dir, "main"), func(m Msg) { msgs = append(msgs, m) }); err != nil {
		return nil, err
	}
	slices.SortStableFunc(msgs, func(a, b Msg) int { return cmp.Compare(a.I, b.I) })
	for _, m := range msgs {
		if m.I != len(c.msgs) {
			warn("main: message %d out of sequence, skipped", m.I)
			continue
		}
		c.msgs = append(c.msgs, m)
	}
	if err := load(filepath.Join(dir, "btw"), func(s Aside) { c.asides = append(c.asides, s) }); err != nil {
		return nil, err
	}
	slices.SortStableFunc(c.asides, func(a, b Aside) int { return a.Date.Compare(b.Date) }) // kept as answered
	if err := load(filepath.Join(dir, "tree"), func(n Node) { c.tree[key{n.L, n.I}] = n.Text }); err != nil {
		return nil, err
	}
	// The view is kept, never rebuilt: a rebuilt view differs and kills every cache entry.
	if b, err := os.ReadFile(filepath.Join(dir, "view.json")); err == nil {
		saved, err := readView(b)
		if err == nil && !tiles(saved.Keys, len(c.msgs)) {
			err = errors.New("its lines don't tile the chat")
		}
		if err != nil {
			warn("view.json: %v; the view starts over from the log", err)
		} else {
			c.view.keys = lo.Map(saved.Keys, func(p [2]int, _ int) key { return key{p[0], p[1]} })
			c.view.shrinking = saved.Shrinking
			c.view.recount(c)
		}
	}
	end := 0
	if n := len(c.view.keys); n > 0 {
		end = c.view.keys[n-1].end()
	}
	for i := end; i < len(c.msgs); i++ { // messages logged after the last save
		c.view.push(key{0, i}, c)
	}
	c.view.check(c) // lines built after the last save
	c.resetCview()
	c.saveView()
	c.enqueueMissing()
	return c, nil
}

// tiles reports whether pairs, as (l, i) keys, cover messages 0 to some
// point at most n, each starting where the last ended.
func tiles(pairs [][2]int, n int) bool {
	end := 0
	for _, p := range pairs {
		k := key{p[0], p[1]}
		if k.L < 0 || k.L > 62 || k.I < 0 || k.start() != end {
			return false
		}
		end = k.end()
	}
	return end <= n
}

// load reads every day file of dir, skipping torn lines.
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

// appendDay appends each v as one line to sub/<today>.jsonl, in one write,
// and fsyncs it.
func (c *Chat) appendDay(sub string, vs ...any) {
	var b []byte
	var err error
	for _, v := range vs {
		line, merr := json.Marshal(v)
		b, err = append(append(b, line...), '\n'), errors.Join(err, merr)
	}
	if err == nil {
		err = appendRaw(filepath.Join(c.dir, sub, time.Now().Format(time.DateOnly)+".jsonl"), b)
	}
	if err != nil {
		// ponytail: a failed write is fatal; the log is history and must not have holes.
		fatal("write %s: %v", sub, err)
	}
}

// Log appends one message, splitting text too long for one. Its images
// (names from saveFile: images and PDFs) go with the first part.
func (c *Chat) Log(kind, text string, images ...string) {
	c.logMsgs([]Msg{{Kind: kind, Text: text, Date: time.Now(), Images: images}})
}

// logRef logs a tool call or its echo, under the call's id.
func (c *Chat) logRef(kind, text, ref string, images ...string) {
	c.logMsgs([]Msg{{Kind: kind, Text: text, Date: time.Now(), Images: images, Ref: ref}})
}

// logMsgs appends messages (their kind, text, date and images) in one
// write, each split as Log splits it.
func (c *Chat) logMsgs(ms []Msg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	from := len(c.msgs)
	var parts []any
	for _, m := range ms {
		for j, part := range splitText(m.Text, clipMax) {
			p := Msg{I: len(c.msgs), Kind: m.Kind, Text: part, Size: len(m.Kind) + 2 + len(part), Date: m.Date, Ref: m.Ref, Part: j}
			if j == 0 {
				p.Images = m.Images
			}
			c.msgs = append(c.msgs, p)
			parts = append(parts, p)
		}
	}
	c.appendDay("main", parts...)
	c.hub.logged(slices.Clone(c.msgs[from:]))
	for i := from; i < len(c.msgs); i++ {
		c.need(key{0, i})
		if c.view.push(key{0, i}, c) {
			c.resetCview()
		} else {
			c.cview.push(key{0, i}, c)
		}
	}
	c.saveView()
	if slices.ContainsFunc(ms, func(m Msg) bool { return m.Kind == "user" }) {
		for _, f := range c.failing { // the user is back: failed compactions are tried again now
			f.until = time.Time{}
		}
	}
	c.pump()
	c.notify()
}

// saveImage stores an image in files/, named by its content, and returns that name.
func (c *Chat) saveImage(b []byte) (string, error) {
	ext, ok := strings.CutPrefix(http.DetectContentType(b), "image/")
	if !ok || !slices.Contains([]string{"png", "jpeg", "gif", "webp"}, ext) {
		return "", fmt.Errorf("not a PNG, JPEG, GIF or WebP image")
	}
	name, _, err := c.saveFile(bytes.NewReader(b), ext)
	return name, err
}

// saveFile stores what r reads in files/, named by its content with the
// extension ext, and returns that name and its size.
func (c *Chat) saveFile(r io.Reader, ext string) (string, int64, error) {
	tmp, name, n, err := c.spool(r, ext)
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(tmp)
	return name, n, c.keep(tmp, name)
}

// spool writes what r reads to a temporary file in files/, and returns its
// path, the name it is kept under (see keep) and its size.
func (c *Chat) spool(r io.Reader, ext string) (tmp, name string, n int64, err error) {
	f, err := os.CreateTemp(filepath.Join(c.dir, "files"), "tmp-*")
	if err != nil {
		return "", "", 0, err
	}
	h := sha256.New()
	n, err = io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if err := errors.Join(err, f.Close()); err != nil {
		os.Remove(f.Name())
		return "", "", 0, err
	}
	name = filepath.Join("files", hex.EncodeToString(h.Sum(nil)[:16]))
	if ext != "" {
		name += "." + ext
	}
	return f.Name(), name, n, nil
}

// keep names the spooled file tmp name, unless a file has that name already:
// the same content, which may be in the log.
func (c *Chat) keep(tmp, name string) error {
	if _, err := os.Stat(filepath.Join(c.dir, name)); err == nil {
		return nil
	}
	return os.Rename(tmp, filepath.Join(c.dir, name))
}

// fileBlocks loads saved images and PDFs as content blocks for a call.
func (c *Chat) fileBlocks(names []string) []block {
	return lo.Map(names, func(name string, _ int) block {
		b, err := os.ReadFile(filepath.Join(c.dir, name))
		if err != nil {
			return textBlock("[file lost: " + name + "]")
		}
		data := base64.StdEncoding.EncodeToString(b)
		if ext := strings.TrimPrefix(filepath.Ext(name), "."); ext != "pdf" {
			return block{Type: "image", Source: &source{Type: "base64", MediaType: "image/" + ext, Data: data}, file: name}
		}
		return block{Type: "document", Source: &source{Type: "base64", MediaType: "application/pdf", Data: data}, file: name}
	})
}

// savedView is view.json: the view's keys as (l, i) pairs, written as a bare
// list unless a batch is cut short, so that a restart goes on with it as an
// unbroken run would. (After https://github.com/jonaslsaa/pi-optchat/pull/180.)
type savedView struct {
	Keys      [][2]int `json:"keys"`
	Shrinking bool     `json:"shrinking"`
}

func readView(b []byte) (savedView, error) {
	var v savedView
	if json.Unmarshal(b, &v.Keys) == nil {
		return v, nil
	}
	return v, json.Unmarshal(b, &v)
}

func (c *Chat) saveView() {
	v := savedView{Keys: lo.Map(c.view.keys, func(k key, _ int) [2]int { return [2]int{k.L, k.I} }), Shrinking: c.view.shrinking}
	var b []byte
	if v.Shrinking {
		b, _ = json.Marshal(v)
	} else {
		b, _ = json.Marshal(v.Keys)
	}
	tmp := filepath.Join(c.dir, "view.json.tmp")
	os.Remove(tmp)
	if err := appendRaw(tmp, b); err != nil { // written and synced before it replaces the old one
		fatal("view.json: %v", err)
	}
	if err := os.Rename(tmp, filepath.Join(c.dir, "view.json")); err != nil {
		fatal("view.json: %v", err)
	}
}

func (c *Chat) resetCview() {
	c.cview.keys = slices.Clone(c.view.keys)
	c.cview.bytes = c.view.bytes
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
	return k.name() + "|" + flat(c.text(k))
}

// ownTags are the tags that frame the view and a compaction's input: inside
// a line (Opt reading this code, a fetched page) one would end the frame.
var ownTags = strings.NewReplacer("<chat>", "‹chat›", "</chat>", "‹/chat›", "<input>", "‹input›", "</input>", "‹/input›")

// flat is a text as one line of the view, its newlines spaces.
func flat(s string) string { return ownTags.Replace(strings.ReplaceAll(s, "\n", " ")) }

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
	c.view.built(c, k)
	c.cview.built(c, k)
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

// cooling says whether line k failed and waits to be tried again.
func (c *Chat) cooling(k key) bool {
	f := c.failing[k]
	return f != nil && time.Now().Before(f.until)
}

// next picks the next node to build: merges first, so that a batch finds the
// parents it needs even behind a backlog, then messages, at most 8 deep, not
// counting those waiting after a failure.
func (c *Chat) next() (key, bool) {
	if k, ok := lo.Find(c.readyM, func(k key) bool { return !c.busy[k] && !c.cooling(k) }); ok {
		return k, true
	}
	n := 0
	for _, i := range c.ready0 {
		k := key{0, i}
		if c.cooling(k) {
			continue
		}
		if n++; n > jobs {
			break
		}
		if !c.busy[k] {
			return k, true
		}
	}
	return key{}, false
}

func (c *Chat) pump() {
	for c.compact != nil && len(c.busy) < jobs { // nil: before the chat starts, or quitting
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
		f := c.failing[k]
		if f == nil {
			f = &failure{}
			c.failing[k] = f
			c.hub.notice("warn", "compaction %s failed, trying again later: %v", k.name(), err)
		}
		if f.n > 0 && c.written > f.at {
			f.strikes++
		}
		f.n, f.at = f.n+1, c.written
		if f.strikes < giveUp {
			wait := min(retryIn<<min(f.n-1, 10), waitMax)
			f.until = time.Now().Add(wait)
			time.AfterFunc(wait, func() {
				c.mu.Lock()
				defer c.mu.Unlock()
				c.pump()
				c.notify()
			})
			c.pump() // the lines after it, in its place
			c.notify()
			return
		}
		// It fails while other lines get built: its source, cut, stands in.
		c.hub.notice("warn", "compaction %s failed %d times while other lines were built: its line is its source, cut and marked: %v", k.name(), f.n, err)
		text = c.fallback(k)
	} else {
		c.written++
	}
	delete(c.failing, k)
	c.setNode(k, text)
	c.refit()
	c.pump()
	c.notify()
}

// refit measures the views again once lines are built: a line's real size
// shows only then, and so do the parents a batch was waiting for.
func (c *Chat) refit() {
	was := c.view.shrinking
	if c.view.check(c) {
		c.resetCview()
		c.saveView()
		return
	}
	c.cview.check(c)
	if c.view.shrinking != was {
		c.saveView()
	}
}

// notSummarized ends a line no model could write, which zoom opens as any other.
const notSummarized = " … (not summarized: zoom it)"

// fallback is line k cut from what it summarizes: the message, or its two
// halves' lines.
func (c *Chat) fallback(k key) string {
	var text string
	if k.L == 0 {
		text = c.msgs[k.I].Kind + ": " + c.msgs[k.I].Text
	} else {
		a, b := k.kids()
		text = c.text(a) + " " + c.text(b)
	}
	return head(strings.Join(strings.Fields(text), " "), nodeMax-len(notSummarized)) + notSummarized
}

var ruler = strings.Repeat("-", nodeMax)

// compactInput is node k's context (the compaction view up to the node, a
// message not summarized yet as its head: eight run at once, and the lines
// after one still tell what it was about) and its task.
func (c *Chat) compactInput(k key) ([]string, []block) {
	until := k.end()
	if k.L == 0 {
		until = k.I
	}
	var keys []key
	for _, v := range c.cview.keys {
		if v.end() > until {
			break
		}
		keys = append(keys, v)
	}
	ctx := lo.Map(keys, func(k key, _ int) string { return c.callLine(k) })
	if k.L == 0 {
		m := c.msgs[k.I]
		in := fmt.Sprintf("Compaction: compress message %d into one line of at most 512 bytes\n"+
			"(about 70 words), the length of this ruler:\n%s\n%s<input>\n%s: %s\n", k.I, ruler, c.callBlock(k.I), m.Kind, ownTags.Replace(m.Text))
		if f := c.failing[k]; len(m.Images) > 0 && f != nil && f.strikes >= giveUp/2 { // they may be what fails
			return ctx, []block{textBlock(in + fmt.Sprintf("[%d attached, which could not be read]\n</input>", len(m.Images)))}
		}
		if len(m.Images) == 0 {
			return ctx, []block{textBlock(in + "</input>")}
		}
		return ctx, append(c.interleave(in, m.Images), textBlock("</input>"))
	}
	a, b := k.kids()
	return ctx, []block{textBlock(fmt.Sprintf("Compaction: merge lines %s and %s, adjacent, into one line of at most\n"+
		"512 bytes (about 70 words), the length of this ruler:\n%s\n"+
		"<chat> may hold their messages, %d to %d, in more detail: take details\nof them from there too.\n"+
		"<input>\n%s\n%s\n</input>",
		a.name(), b.name(), ruler, k.start(), k.end()-1, c.line(a), c.line(b)))}
}

const callMax = 4000 // bytes of a tool call that a compaction of its result sees

var callTags = strings.NewReplacer("<call>", "‹call›", "</call>", "‹/call›")

// callBlock is, for a tool result, the call it answers, which its line may
// not say well enough to tell what the result is (an Edit cut at 512
// bytes): context, ahead of the input. The call is the message of the same
// Ref, or with none (logged before Refs were kept), found by its place.
// (After https://github.com/jonaslsaa/pi-optchat/pull/171.)
func (c *Chat) callBlock(i int) string {
	m := c.msgs[i]
	if m.Kind != "echo" {
		return ""
	}
	j := -1
	if m.Ref == "" {
		j = c.callByPlace(i)
	} else {
		for k := i - 1; k >= 0 && k >= i-1000; k-- {
			if c.msgs[k].Kind == "tool" && c.msgs[k].Ref == m.Ref {
				j = k // its first part, if it was split
			}
		}
	}
	if j < 0 {
		return ""
	}
	call := c.msgs[j].Text
	if cut := head(call, callMax); cut != call {
		call = cut + "…"
	}
	return fmt.Sprintf("<call> is the tool call that message %d answers (message %d, which has its own\n"+
		"line): context, as <chat> is.\n<call>\n%s\n</call>\n", i, j, callTags.Replace(ownTags.Replace(call)))
}

// callByPlace finds the call a result with no Ref answers: calls made at
// once come in a run, and their results in the next, in the same order, so
// the kth result answers the kth call when both runs are as long. Runs are
// counted in messages, not their parts. -1 when that does not hold.
func (c *Chat) callByPlace(i int) int {
	run := func(j int, kind string) bool {
		return j >= 0 && j < len(c.msgs) && c.msgs[j].Kind == kind && c.msgs[j].Ref == ""
	}
	starts := func(from, to int) []int { // the messages from..to-1 start, as indexes
		var s []int
		for j := from; j < to; j++ {
			if c.msgs[j].Part == 0 {
				s = append(s, j)
			}
		}
		return s
	}
	from, to := i, i+1
	for run(from-1, "echo") {
		from--
	}
	for run(to, "echo") {
		to++
	}
	calls := from
	for run(calls-1, "tool") {
		calls--
	}
	results := starts(from, to)
	k := slices.IndexFunc(results, func(j int) bool { return j > i }) // i's message is the one before
	if k < 0 {
		k = len(results)
	}
	if made := starts(calls, from); len(made) == len(results) && k > 0 {
		return made[k-1]
	}
	return -1
}

func (c *Chat) notify() {
	close(c.changed)
	c.changed = make(chan struct{})
}

const settleMax = 32 // a call waits for every line when at most this many are to build: four rounds of jobs...

var settleWait = time.Minute // ...and for at most this long

var retryIn = 10 * time.Second // a failed compaction's first wait, doubled at each failure

// settle waits until every message is summarized, so that no call sees a
// placeholder, as the recipe says, when that is a short wait. Behind a bigger
// backlog (an import), or after settleWait, it returns at once, and the call
// sees the tree as far as it is built (callView), as in UniiChat: the chat
// never waits hours. It tells waiting how many messages are left, and how
// many lines a model has written since it began, at each change.
func (c *Chat) settle(ctx context.Context, waiting func(n, written int)) error {
	c.mu.Lock()
	from := c.written
	c.mu.Unlock()
	timeout := time.NewTimer(settleWait)
	defer timeout.Stop()
	for {
		c.mu.Lock()
		n, written, ch := len(c.ready0), c.written-from, c.changed
		big := len(c.ready0)+len(c.readyM) > settleMax
		_, more := c.next()
		failing := len(c.busy) == 0 && !more && c.compact != nil // all that is left failed: no wait for its next try
		c.mu.Unlock()
		if n == 0 || big || failing {
			return nil
		}
		waiting(n, written)
		select {
		case <-ch:
		case <-timeout.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// callView is the view as a turn or subagent sees it. With every line built,
// it is the view itself, byte for byte, so the cache holds. Behind a backlog
// (see settle) a message not summarized yet shows its head, and once that
// passes viewHi, runs of lines not built yet, then any lines, oldest first,
// show as their parents, built or not, down to viewLo: a partial tree that
// zoom opens like any other. Caller holds c.mu.
func (c *Chat) callView() []string {
	keys := c.view.keys
	if !slices.ContainsFunc(keys, func(k key) bool { return !c.built(k) }) {
		return c.lines(keys)
	}
	size := lo.SumBy(keys, func(k key) int { return len(c.callLine(k)) + 1 })
	for _, anyPair := range []bool{false, true} {
		for merged := true; merged && size > viewHi; {
			merged = false
			out := make([]key, 0, len(keys))
			for j := 0; j < len(keys); j++ {
				a := keys[j]
				if j+1 < len(keys) && size > viewLo {
					b := keys[j+1]
					if a.L == b.L && a.I%2 == 0 && b.I == a.I+1 && (anyPair || !c.built(a) && !c.built(b)) {
						p := a.parent()
						size += len(c.callLine(p)) - len(c.callLine(a)) - len(c.callLine(b)) - 1
						out = append(out, p)
						j++
						merged = true
						continue
					}
				}
				out = append(out, a)
			}
			keys = out
		}
	}
	return lo.Map(keys, func(k key, _ int) string { return c.callLine(k) })
}

// callLine is line k as a call sees it: a message not summarized yet shows
// its head, up to nodeMax bytes.
func (c *Chat) callLine(k key) string {
	if k.L > 0 || c.built(k) {
		return c.line(k)
	}
	m := c.msgs[k.I]
	text := flat(m.Text)
	if cut := head(text, nodeMax); cut != text {
		text = cut + "…"
	}
	if n := len(m.Images); n > 0 {
		text += fmt.Sprintf(" [%d attached]", n)
	}
	return fmt.Sprintf("%s|%s: %s %s", k.name(), m.Kind, text, unbuilt)
}

// Zoom opens line id+n into its two halves; n = 1 gives the message whole,
// with its images.
func (c *Chat) Zoom(id, n int) (string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n < 1 || n&(n-1) != 0 || id < 0 || id%n != 0 || id > len(c.msgs)-n {
		return fmt.Sprintf("No line %d+%d.", id, n), nil
	}
	if n == 1 {
		m := c.msgs[id]
		return fmt.Sprintf("%d+1|%s: %s", id, m.Kind, m.Text), m.Images
	}
	a, b := key{bits.TrailingZeros(uint(n)), id / n}.kids()
	return c.callLine(a) + "\n" + c.callLine(b), nil // a message not summarized yet: its head
}

const (
	searchPage  = 20
	searchWords = 10 // messages a search that finds nothing shows by its words instead
)

// Search finds the messages that hold text, in any case, newest first, a
// page at a time, from before message before (0: the end): each with a
// snippet, its date, and the view's line that holds it, to zoom into. The
// chat's own zoom, search and date calls and their results are left out:
// they only repeat what they found.
func (c *Chat) Search(text string, before int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "No text given."
	}
	c.mu.Lock()
	msgs := c.msgs // logged messages never change: read outside c.mu
	c.mu.Unlock()
	if before <= 0 || before > len(msgs) {
		before = len(msgs)
	}
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(text))
	lookups := lookupRefs(msgs)
	var hits []int
	total := 0
	for i := before - 1; i >= 0; i-- {
		if m := msgs[i]; !lookups[m.Ref] && re.MatchString(m.Text) {
			if total++; len(hits) < searchPage {
				hits = append(hits, i)
			}
		}
	}
	if total == 0 {
		// The chat often put it in other words: rank the messages that
		// share its words, as recall does. On questions worded apart from
		// the chat, this answered more of them right at the same cost
		// (docs/2026-10-19-memory-eval-2.md).
		if lines := c.shared(text, before, searchWords); lines != "" {
			return fmt.Sprintf("No message before %d holds %q. These share its words, rarest weighing most, "+
				"each cut to %d bytes around its rarest word: zoom(id, 1) opens one whole.\n", before, text, recallCut) + strings.TrimRight(lines, "\n")
		}
		return fmt.Sprintf("No message before %d holds %q.", before, text)
	}
	var b strings.Builder
	holds := "messages hold"
	if total == 1 {
		holds = "message holds"
	}
	fmt.Fprintf(&b, "%d %s %q; the %d newest before %d:\n", total, holds, text, len(hits), before)
	c.mu.Lock()
	for _, i := range hits {
		m := msgs[i]
		at := re.FindStringIndex(m.Text)
		from, to := max(0, at[0]-100), min(len(m.Text), at[1]+150)
		for from > 0 && !utf8.RuneStart(m.Text[from]) {
			from--
		}
		for to < len(m.Text) && !utf8.RuneStart(m.Text[to]) {
			to++
		}
		snip := flat(m.Text[from:to])
		if from > 0 {
			snip = "…" + snip
		}
		if to < len(m.Text) {
			snip += "…"
		}
		fmt.Fprintf(&b, "%d+1|%s: %s (%s; in line %s)\n", i, m.Kind, snip, m.Date.Local().Format("2006-01-02"), c.viewLine(i).name())
	}
	c.mu.Unlock()
	if total > len(hits) {
		fmt.Fprintf(&b, "More: search(%q, %d).", text, hits[len(hits)-1])
	}
	return strings.TrimRight(b.String(), "\n")
}

// lookupRefs are the Refs of the chat's own zoom, search and date calls,
// whose results only repeat what they found.
func lookupRefs(msgs []Msg) map[string]bool {
	refs := map[string]bool{}
	for _, m := range msgs {
		if w, _, _ := strings.Cut(m.Text, " "); m.Kind == "tool" && m.Ref != "" && (w == "zoom" || w == "search" || w == "date") {
			refs[m.Ref] = true
		}
	}
	return refs
}

const recallCut = 1200 // bytes of a message that recall shows

var (
	nonWord   = regexp.MustCompile(`[^\p{L}\p{M}\p{N}]+`)
	recallTag = strings.NewReplacer("<recall>", "‹recall›", "</recall>", "‹/recall›")
)

// Recall finds the limit messages before message before that best share
// the words of text, rare words weighing most, with no model call, as a
// block for a turn's input; "" when none does. A summary line keeps only
// so much of a message, so a turn gets these raw: names, numbers, paths.
// Each word of three characters or more weighs its BM25 inverse document
// frequency squared, so that the rare words lead, and nothing when half
// the messages or more hold it. A message scores the sum of the weights of
// the words it holds, scaled by its length as BM25 scales a word found
// once, so that a long paste holding many middling words loses to a short
// message holding the rare one. Ties go to the newer; hits under a quarter
// of the best one's score are dropped. The chat's own lookups are left
// out, and so are messages the view shows whole.
// (After https://github.com/ericjuta/omp-optchat/releases/tag/v0.9.0,
// whose recall tool takes such messages as candidates.)
func (c *Chat) Recall(text string, before, limit int) string {
	lines := c.shared(text, before, limit)
	if lines == "" {
		return ""
	}
	return fmt.Sprintf("[opt-chat] Recall: messages from the log that share this message's rarest words,\n"+
		"found by words alone, so some may be beside the point; each cut to %d bytes\n"+
		"around its rarest word: zoom(id, 1) opens one whole.\n<recall>\n", recallCut) + lines + "</recall>\n\n"
}

// shared is Recall's messages, a line each; "" when none.
func (c *Chat) shared(text string, before, limit int) string {
	var terms []string
	for _, w := range nonWord.Split(strings.ToLower(text), -1) {
		if utf8.RuneCountInString(w) >= 3 && !slices.Contains(terms, w) && len(terms) < 64 {
			terms = append(terms, w)
		}
	}
	if len(terms) == 0 || limit <= 0 {
		return ""
	}
	c.mu.Lock()
	msgs := c.msgs // logged messages never change: read outside c.mu
	c.mu.Unlock()
	before = min(before, len(msgs))
	lookups := lookupRefs(msgs[:before])
	df := make([]int, len(terms))
	type hit struct {
		i     int
		terms []int
		score float64
	}
	var hits []hit
	n, total := 0, 0
	for i := range before {
		m := msgs[i]
		if lookups[m.Ref] {
			continue
		}
		n, total = n+1, total+len(m.Text)
		lower := strings.ToLower(m.Text)
		var found []int
		for t, term := range terms {
			if strings.Contains(lower, term) {
				df[t]++
				found = append(found, t)
			}
		}
		if found != nil {
			hits = append(hits, hit{i: i, terms: found})
		}
	}
	weight := lo.Map(df, func(d, _ int) float64 { w := max(0, math.Log((float64(n-d)+0.5)/(float64(d)+0.5))); return w * w })
	c.mu.Lock()
	// Shown whole: a message's own line, written with no model call. Behind
	// a backlog, the call may see its parent instead (callView): kept then.
	if !slices.ContainsFunc(c.view.keys, func(k key) bool { return !c.built(k) }) {
		hits = slices.DeleteFunc(hits, func(h hit) bool {
			return c.viewLine(h.i).L == 0 && msgs[h.i].Size <= nodeMax && len(msgs[h.i].Images) == 0
		})
	}
	c.mu.Unlock()
	// k1 is BM25's usual. Its usual b, 0.75, buried long replies and
	// reports: on questions drawn from a real chat, 0.3 put the message
	// holding the answer in the first 8 for 40 of 56 against 32, and the
	// calls answering them made 40% fewer tool calls
	// (docs/2026-10-17-memory-eval.md).
	const k1, lenB = 1.2, 0.3
	avg := float64(total) / float64(max(n, 1))
	for j, h := range hits {
		norm := (k1 + 1) / (1 + k1*(1-lenB+lenB*float64(len(msgs[h.i].Text))/avg))
		hits[j].score = norm * lo.SumBy(h.terms, func(t int) float64 { return weight[t] })
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.i, a.i)) })
	if len(hits) == 0 || hits[0].score <= 0 {
		return ""
	}
	// Far behind the best, a hit shares only middling words: noise.
	top := hits[0].score
	hits = slices.DeleteFunc(hits, func(h hit) bool { return h.score < top/4 })
	var b strings.Builder
	for _, h := range hits[:min(limit, len(hits))] {
		m := msgs[h.i]
		rare := slices.MaxFunc(h.terms, func(a, b int) int { return cmp.Compare(weight[a], weight[b]) })
		fmt.Fprintf(&b, "%d+1|%s, %s: %s\n", h.i, m.Kind, m.Date.Local().Format("2006-01-02"), recallTag.Replace(flat(excerpt(m.Text, terms[rare], recallCut))))
	}
	return b.String()
}

// excerpt is up to n bytes of s around the first place it holds word, in
// any case, marked … where cut.
func excerpt(s, word string, n int) string {
	if len(s) <= n {
		return s
	}
	at := 0
	if loc := regexp.MustCompile("(?i)" + regexp.QuoteMeta(word)).FindStringIndex(s); loc != nil {
		at = loc[0]
	}
	from := min(max(0, at-n/3), len(s)-n)
	for from > 0 && !utf8.RuneStart(s[from]) {
		from--
	}
	out := head(s[from:], n)
	if from+len(out) < len(s) {
		out += "…"
	}
	if from > 0 {
		out = "…" + out
	}
	return out
}

// viewLine is the view's line holding message i. Caller holds c.mu.
func (c *Chat) viewLine(i int) key {
	j, _ := slices.BinarySearchFunc(c.view.keys, i, func(k key, i int) int {
		switch {
		case k.end() <= i:
			return -1
		case k.start() > i:
			return 1
		}
		return 0
	})
	if j < len(c.view.keys) {
		return c.view.keys[j]
	}
	return key{0, i}
}

// viewNow is the view as a call would see it now.
func (c *Chat) viewNow() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callView()
}

func (c *Chat) Date(id int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id < 0 || id >= len(c.msgs) {
		return fmt.Sprintf("No message %d.", id)
	}
	return c.msgs[id].Date.Local().Format("Mon 2006-01-02 15:04:05 MST")
}

// splitText cuts s into pieces of at most n characters, each ending at a
// line break when one is in reach.
func splitText(s string, n int) []string {
	var out []string
	for utf8.RuneCountInString(s) > n {
		i := runeOffset(s, n)
		if j := strings.LastIndexByte(s[:i], '\n'); j > 0 {
			i = j + 1
		}
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
