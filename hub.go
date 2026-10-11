package main

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
)

// hub carries what happens in the chat, as it happens, to whoever shows it:
// each web page, over its WebSocket, and the terminal. It also holds what a
// page opened mid-turn needs to catch up: the turn underway, with the reply
// being written, and the queue.
type hub struct {
	mu      sync.Mutex
	clients map[*client]bool
	turn    liveTurn
	queue   []queueItem
	asks    []ask // tool calls waiting for the user's permission
	status  status
	last    *turnEnd
	print   func(any) // the terminal's share of each update; nil: none
}

// client is one page's outbox. A page that falls behind is dropped, and
// catches up when it reconnects.
type client struct {
	out    chan []byte
	closed bool
}

const outbox = 4096

func newHub() *hub { return &hub{clients: map[*client]bool{}} }

// Updates, each with its type in t.
type (
	msgsUpdate struct {
		T    string `json:"t"` // msgs
		Msgs []Msg  `json:"msgs"`
	}
	deltaUpdate struct {
		T string `json:"t"` // delta
		K string `json:"k"` // text or thinking
		S string `json:"s"`
		R bool   `json:"r,omitzero"` // a new block: empty it first
	}
	turnUpdate struct {
		T    string   `json:"t"` // turn
		Turn liveTurn `json:"turn"`
	}
	queueUpdate struct {
		T     string      `json:"t"` // queue
		Queue []queueItem `json:"queue"`
	}
	asksUpdate struct {
		T    string `json:"t"` // asks
		Asks []ask  `json:"asks"`
	}
	statusUpdate struct {
		T      string `json:"t"` // status
		Status status `json:"status"`
	}
	noticeUpdate struct {
		T     string `json:"t"` // notice
		Level string `json:"level"`
		S     string `json:"s"`
	}
	thoughtUpdate struct {
		T string `json:"t"` // thought: a whole thinking block
		S string `json:"s"`
	}
	endUpdate struct {
		T   string  `json:"t"` // end: a turn's usage
		End turnEnd `json:"end"`
	}
	helloUpdate struct {
		T      string      `json:"t"` // hello
		Msgs   []Msg       `json:"msgs"`
		Total  int         `json:"total"`
		Reset  bool        `json:"reset"` // msgs are the last page, not the ones after the page's last
		Turn   liveTurn    `json:"turn"`
		Queue  []queueItem `json:"queue"`
		Asks   []ask       `json:"asks"`
		Status status      `json:"status"`
		Last   *turnEnd    `json:"last"`
		Inputs []string    `json:"inputs"` // the user's last messages, for Up
	}
)

// liveTurn is the turn underway as a page shows it.
type liveTurn struct {
	Phase    string `json:"phase"`            // "" idle, settling, reading, thinking, writing, tool
	Since    int64  `json:"since,omitzero"`   // when it started, Unix ms
	Tokens   int    `json:"tokens,omitzero"`  // output tokens so far
	Tool     string `json:"tool,omitzero"`    // phase tool: the one running
	Backlog  int    `json:"backlog,omitzero"` // settling: messages left to summarize
	Written  int    `json:"written,omitzero"` // settling: lines written since it began
	Partial  int    `json:"partial,omitzero"` // messages the turn sees unsummarized
	From     int    `json:"from"`             // the turn's first message: a tool call after it, unanswered, is running
	Text     string `json:"text,omitzero"`    // in a hello only: the reply being written
	Thinking string `json:"thinking,omitzero"`
	Input    string `json:"input,omitzero"` // the tool call's input as it is written: JSON, partial
}

type queueItem struct {
	ID    int      `json:"id"`
	Text  string   `json:"text"`
	Files []string `json:"files,omitzero"`
}

type status struct {
	Msgs      int      `json:"msgs"`
	ViewLines int      `json:"viewLines"`
	ViewBytes int      `json:"viewBytes"`
	Backlog   int      `json:"backlog"` // messages to summarize
	Merges    int      `json:"merges"`  // merges to build
	Running   int      `json:"running"` // compactions running
	Failing   int      `json:"failing"` // waiting to be tried again
	Written   int      `json:"written"` // lines built in this run
	Model     string   `json:"model"`
	Effort    string   `json:"effort"`
	Mode      string   `json:"mode"` // the permission mode
	Agents    []string `json:"agents"`
	Dir       string   `json:"dir"`
	Cwd       string   `json:"cwd"` // where Opt's tools run
}

type turnEnd struct {
	View        usage `json:"view"`
	Turn        usage `json:"turn"`
	Compactions int   `json:"compactions"`
	CUsage      usage `json:"cusage"`
	At          int64 `json:"at"`
}

func (u turnEnd) String() string {
	return fmt.Sprintf("[view: %s | turn: %s | %d compactions: %s]", u.View, u.Turn, u.Compactions, u.CUsage)
}

// send hands v to every page and to the terminal. The caller may hold
// Chat.mu: the hub never takes it.
func (h *hub) send(v any) {
	if h == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		warn("hub: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sendLocked(v, b)
}

// sendLocked sends v, b its JSON, with h.mu held: what changed the hub's
// state goes out in the same hold, so a page that joins gets each change
// once, in its hello or as an update.
func (h *hub) sendLocked(v any, b []byte) {
	h.fanout(b)
	if h.print != nil {
		h.print(v)
	}
}

// change applies f to the hub's state and sends v in one hold of h.mu.
func (h *hub) change(f func(), v any) {
	if h == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		warn("hub: %v", err)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	f()
	h.sendLocked(v, b)
}

func (h *hub) fanout(b []byte) {
	for cl := range h.clients {
		select {
		case cl.out <- b:
		default: // too far behind: it reconnects and catches up
			h.drop(cl)
		}
	}
}

func (h *hub) drop(cl *client) {
	if !cl.closed {
		cl.closed = true
		close(cl.out)
	}
	delete(h.clients, cl)
}

func (h *hub) leave(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.drop(cl)
}

// join adds a page. Its first update is a hello with the messages after its
// last (after; -1 for none), or the last page of them when that is fewer.
func (h *hub) join(c *Chat, after, page int) *client {
	c.mu.Lock()
	defer c.mu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	from := after + 1
	reset := after < 0 || after >= len(c.msgs) || len(c.msgs)-from > page
	if reset {
		from = c.whole(max(0, len(c.msgs)-page))
	}
	hello := helloUpdate{T: "hello", Msgs: c.msgs[from:], Total: len(c.msgs), Reset: reset, Turn: h.turn,
		Queue: h.queue, Asks: h.asks, Status: h.status, Last: h.last, Inputs: c.recentInputs(100)}
	if hello.Queue == nil {
		hello.Queue = []queueItem{}
	}
	if hello.Asks == nil {
		hello.Asks = []ask{}
	}
	b, err := json.Marshal(hello)
	cl := &client{out: make(chan []byte, outbox)}
	if err != nil {
		warn("hello: %v", err)
		close(cl.out)
		cl.closed = true
		return cl
	}
	cl.out <- b
	h.clients[cl] = true
	return cl
}

// whole moves i back to the first part of its message, so that a page of
// the log starts with a whole one. Call with c.mu held.
func (c *Chat) whole(i int) int {
	for i > 0 && i < len(c.msgs) && c.msgs[i].Part > 0 {
		i--
	}
	return i
}

func (h *hub) logged(ms []Msg) {
	h.send(msgsUpdate{T: "msgs", Msgs: ms})
}

// delta adds to the reply or the thinking being written.
func (h *hub) delta(k, s string) {
	if s == "" {
		return
	}
	h.change(func() {
		switch k {
		case "text":
			h.turn.Text += s
		case "input":
			h.turn.Input += s
		default:
			h.turn.Thinking += s
		}
	}, deltaUpdate{T: "delta", K: k, S: s})
}

// setTurn changes the turn underway and tells the pages; the text and
// thinking travel as deltas.
func (h *hub) setTurn(f func(*liveTurn)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	before := h.turn
	f(&h.turn)
	t := h.turn
	h.mu.Unlock()
	t.Text, t.Thinking, t.Input = "", "", ""
	before.Text, before.Thinking, before.Input = "", "", ""
	if t != before {
		h.send(turnUpdate{T: "turn", Turn: t})
	}
}

// restart empties the reply or the thinking being written, as a new block
// of it starts.
func (h *hub) restart(k string) {
	h.change(func() {
		switch k {
		case "text":
			h.turn.Text = ""
		case "input":
			h.turn.Input = ""
		default:
			h.turn.Thinking = ""
		}
	}, deltaUpdate{T: "delta", K: k, R: true})
}

// clearText drops the reply being written as it goes into the log, which
// the pages take as the end of it.
func (h *hub) clearText() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.turn.Text = ""
	h.mu.Unlock()
}

// clearInput drops the tool call's input being written as the call goes
// into the log, which the pages take as the end of it.
func (h *hub) clearInput() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.turn.Input = ""
	h.mu.Unlock()
}

func (h *hub) setQueue(q []queueItem) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.queue = slices.Clone(q)
	h.mu.Unlock()
	if q == nil {
		q = []queueItem{}
	}
	h.send(queueUpdate{T: "queue", Queue: q})
}

func (h *hub) setAsks(asks []ask) {
	if h == nil {
		return
	}
	h.change(func() { h.asks = asks }, asksUpdate{T: "asks", Asks: asks})
}

func (h *hub) setStatus(s status) {
	if h == nil {
		return
	}
	h.mu.Lock()
	same := reflect.DeepEqual(h.status, s)
	h.status = s
	h.mu.Unlock()
	if !same {
		h.send(statusUpdate{T: "status", Status: s})
	}
}

func (h *hub) ended(e turnEnd) {
	if h == nil {
		dim(e.String())
		return
	}
	e.At = time.Now().UnixMilli()
	h.mu.Lock()
	h.last = &e
	h.mu.Unlock()
	h.send(endUpdate{T: "end", End: e})
}

// notice tells the pages and the terminal something outside the chat: level
// warn or info.
func (h *hub) notice(level, f string, a ...any) {
	s := fmt.Sprintf(f, a...)
	if h == nil || h.print == nil {
		if level == "warn" {
			warn("%s", s)
		} else {
			dim(s)
		}
	}
	h.send(noticeUpdate{T: "notice", Level: level, S: s})
}

func (h *hub) thought(s string) {
	if h == nil {
		dim(s)
		return
	}
	// Done thinking: a page that joins from now on is not shown it as underway.
	h.change(func() { h.turn.Thinking = "" }, thoughtUpdate{T: "thought", S: s})
}

// recentInputs is the user's last n messages, newest last, without
// attachments' additions. Caller holds c.mu.
func (c *Chat) recentInputs(n int) []string {
	var out []string
	for i := len(c.msgs) - 1; i >= 0 && len(out) < n; i-- {
		if m := c.msgs[i]; m.Kind == "user" && m.Part == 0 {
			if s := stripAttachments(m.Text); s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	slices.Reverse(out)
	return out
}

// watchStatus keeps the pages' status bar current: at every change in the
// chat, at most four times a second, and each second.
func watchStatus(c *Chat, a *Agent, h *hub, stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		h.setStatus(currentStatus(c, a))
		c.mu.Lock()
		ch := c.changed
		c.mu.Unlock()
		select {
		case <-ch:
		case <-tick.C:
		case <-stop:
			return
		}
		select {
		case <-time.After(250 * time.Millisecond):
		case <-stop:
			return
		}
	}
}

func currentStatus(c *Chat, a *Agent) status {
	a.mu.Lock()
	s := status{Model: a.model, Effort: a.effort, Mode: a.perm, Agents: sortedKeys(a.running), Cwd: a.cwd}
	a.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	s.Msgs, s.ViewLines, s.ViewBytes = len(c.msgs), len(c.view.keys), c.view.size()
	s.Backlog, s.Merges, s.Running, s.Failing, s.Written = len(c.ready0), len(c.readyM), len(c.busy), len(c.failing), c.written
	s.Dir = c.dir
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
