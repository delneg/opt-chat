package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/samber/lo"
)

// A subagent's chat is memory too: a Chat of its own in agents/<Name>.mem,
// fed from its log, with the main chat's tree, compactions and zoom. A long
// subagent's log runs to megabytes; its view stays within agentViewHi, and
// zoom opens it down to whole steps, so a fresh agent or Opt learns what it
// did without reading the whole log. (A summary tree per session, subagents
// included, as https://github.com/ahlner/opencode-optchat keeps.)
const (
	agentViewLo = 16_000 // a subagent's view drops back to this...
	agentViewHi = 32_000 // ...once it passes this: one zoom
	// Compactions running at once in all subagents' chats together, unless
	// -agent-jobs says otherwise.
	defaultAgentJobs = 2
	// Subagents' chats kept open at most: past it, the least used idle ones
	// are closed, and opened again from their files when next needed.
	agentOpenMax = 32
)

// agentMems are the subagents' chats opened so far: a running subagent's
// is fed as it writes; a finished one's, at its first zoom, from its whole
// log. A nil *agentMems is -agent-memory=false: steps go to the log only,
// and zoom("Name") gives the whole log.
//
// Locks, always taken in this order: Agent.mu (tell, tellNow and logTells
// log a step holding it), then an agentMem's mu, then agentMems.mu, then a
// Chat's mu, then jobGate.mu. closeIdleAgents, which holds
// agentMems.mu, only tries an agentMem's mu, so it never waits against the
// order.
type agentMems struct {
	main    *Chat // its dir, and its hub for the subagents' chats' warnings
	mu      sync.Mutex
	compact compactor // the main chat's; nil while quitting
	open    map[string]*agentMem
	gate    jobGate
}

// newAgentMems sets subagents' memory up beside the main chat, which then
// holds their compactions back while its own wait.
func newAgentMems(main *Chat, jobs int) *agentMems {
	m := &agentMems{main: main, gate: jobGate{max: jobs}}
	m.gate.kick = m.pump
	main.mu.Lock()
	main.role = chatRole{gate: &m.gate}
	main.mu.Unlock()
	return m
}

// agentMem is one subagent's chat. Its mu is held over a step's write to
// the log and its feed into the chat, and over a zoom's feed, so a feed
// never reads a step half written.
type agentMem struct {
	mu   sync.Mutex
	c    *Chat        // set holding both mu and agentMems.mu: read holding either
	read int64        // bytes of the log fed into c
	used atomic.Int64 // when last fed, in Unix nanoseconds: read without mu
	gone bool         // closed and out of the map: take the name's entry again
	// The log ends in a blank line, as each step does: written by this
	// entry. Not known, it is checked before a step: one cut short (a
	// crash, a full disk) would take the next step's head for its text.
	sealed bool
	// Its failure was said: a write that fails again says nothing until
	// one works. Read without mu, by its chat's failWrite.
	warned atomic.Bool
}

// chatRole sets a chat's compactions apart by whose chat it is: the main
// chat's (sub "") go first; a subagent's share gate's -agent-jobs, and its
// warnings go to the main chat's page (notices), named. Read holding the
// chat's mu.
type chatRole struct {
	sub     string       // the subagent's name; "": the main chat
	gate    *jobGate     // nil: the main chat with -agent-memory=false
	notices *hub         // a subagent's chat's: the main chat's hub (its messages are not the page's)
	warned  *atomic.Bool // a subagent's chat's: its failure was said (agentMem.warned)
}

// take is whether a compaction may start; done, that one taken has ended.
func (r chatRole) take() bool { return r.sub == "" || r.gate.take() }
func (r chatRole) done() {
	if r.sub != "" {
		r.gate.give()
	}
}

// pumped tells the gate, after the main chat's pump, whether its lines wait.
func (r chatRole) pumped(waits bool) {
	if r.sub == "" && r.gate != nil {
		r.gate.setMain(waits)
	}
}

// mergedHeads is whether a merged line not built shows its messages'
// heads: a subagent's first zoom often finds none built.
func (r chatRole) mergedHeads() bool { return r.sub != "" }

// whose is what a compaction is told of whose chat it is.
func (r chatRole) whose() string {
	if r.sub == "" {
		return ""
	}
	return fmt.Sprintf("This is subagent [%s]'s own chat, not the main one. Its kinds: task, its task; "+
		"on, its model; agent, its replies; tool and echo, its own tool calls and their results; "+
		"opt and user, messages Opt or the user sent it; cut, compacted and loop, opt-chat's notes.\n", r.sub)
}

// jobGate shares -agent-jobs compactions among all subagents' chats, and
// opens none while the main chat has a line waiting to be built: the main
// chat's come first.
type jobGate struct {
	mu        sync.Mutex
	max, busy int
	mainWaits bool
	kick      func() // pumps every subagent's chat, without locks held
}

func (g *jobGate) take() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.mainWaits || g.busy >= g.max {
		return false
	}
	g.busy++
	return true
}

func (g *jobGate) give() {
	g.mu.Lock()
	g.busy--
	g.mu.Unlock()
	go g.kick()
}

// setMain is whether the main chat has lines waiting, from its pump.
func (g *jobGate) setMain(waits bool) {
	g.mu.Lock()
	was := g.mainWaits
	g.mainWaits = waits
	g.mu.Unlock()
	if was && !waits {
		go g.kick()
	}
}

// pump gives the subagents' chats a chance at the gate. It takes their
// mus without agentMems.mu: one held over a write to disk holds up no
// other subagent. One closed meanwhile has no compactor, and pumps nothing.
func (m *agentMems) pump() {
	m.mu.Lock()
	var cs []*Chat
	for _, am := range m.open {
		if am.c != nil {
			cs = append(cs, am.c)
		}
	}
	m.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		c.pump()
		c.mu.Unlock()
	}
}

// setCompact sets the compactor of every subagent's chat, open or to open,
// and sets them building: at the start, at a quit (nil) and around a
// reload.
func (m *agentMems) setCompact(f compactor) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.compact = f
	for _, am := range m.open {
		if am.c != nil {
			am.c.mu.Lock()
			am.c.compact = f
			am.c.pump()
			am.c.mu.Unlock()
		}
	}
}

// lock is the subagent name's entry, made if need be, its mu held.
func (m *agentMems) lock(name string) *agentMem {
	for {
		m.mu.Lock()
		am := m.open[name]
		if am == nil {
			am = &agentMem{}
			if m.open == nil {
				m.open = map[string]*agentMem{}
			}
			m.open[name] = am
		}
		m.mu.Unlock()
		am.mu.Lock()
		if !am.gone {
			return am
		}
		am.mu.Unlock()
	}
}

// appendStep writes b, a step, to the log of the subagent name at path
// and feeds it to its chat, holding the subagent's lock over both.
func (m *agentMems) appendStep(name, path string, b []byte) error {
	if m == nil {
		return appendRaw(path, sealed(path, b)) // memory may be on after a restart
	}
	am := m.lock(name)
	if !am.sealed {
		b = sealed(path, b)
	}
	err := appendRaw(path, b)
	am.sealed = err == nil
	if err == nil {
		if _, ferr := m.fed(name, am); ferr != nil && !errors.Is(ferr, errMemBroken) && !am.warned.Swap(true) { // that one is said as it happens
			m.main.hub.notice("warn", "[%s] memory: %v", name, ferr)
		}
	}
	am.mu.Unlock()
	m.closeIdle(name)
	return err
}

// chat is the chat of the subagent name, opened if need be, with every
// step of its log in it.
func (m *agentMems) chat(name string) (*Chat, error) {
	am := m.lock(name)
	sc, err := m.fed(name, am)
	am.mu.Unlock()
	m.closeIdle(name)
	return sc, err
}

// fed opens the subagent's chat if need be and feeds it what its log
// gained since. am.mu held. Each step goes in once: a chat opened again
// skips the steps it holds, counted as its messages' first parts, so a
// crash between the log and the chat loses none and doubles none.
func (m *agentMems) fed(name string, am *agentMem) (*Chat, error) {
	am.used.Store(time.Now().UnixNano())
	m.closeBroken(am) // a compaction's write failed since: open it again from its files
	if sc := am.c; sc != nil {
		err := cmp.Or(m.feed(name, am, sc), m.closeBroken(am))
		if err == nil {
			am.warned.Store(false)
		}
		return sc, err
	}
	dir := filepath.Join(m.main.dir, "agents")
	if _, err := os.Stat(filepath.Join(dir, name+".log")); os.IsNotExist(err) {
		return nil, errNoLog
	} else if err != nil {
		return nil, err
	}
	sc, err := openChatAs(filepath.Join(dir, name+".mem"), chatRole{sub: name, gate: &m.gate, notices: m.main.hub, warned: &am.warned})
	if err != nil {
		return nil, err
	}
	sc.view.lo, sc.view.hi = agentViewLo, agentViewHi
	sc.mu.Lock()
	sc.view.check(sc)
	sc.mu.Unlock()
	if err := m.feed(name, am, sc); err != nil || sc.broken != nil { // before its first pump: the context it builds on is whole
		sc.lock.Close()
		am.read = 0
		if sc.broken != nil {
			return nil, fmt.Errorf("%w: %v", errMemBroken, sc.broken)
		}
		return nil, err
	}
	m.mu.Lock()
	sc.mu.Lock()
	sc.compact = m.compact
	sc.pump()
	sc.mu.Unlock()
	am.c = sc
	m.mu.Unlock()
	am.warned.Store(false)
	return sc, nil
}

// closeBroken closes the subagent's chat if a write to it failed, to be
// opened again from its files, and says why. am.mu held.
func (m *agentMems) closeBroken(am *agentMem) error {
	if am.c == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sc := am.c
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.broken == nil {
		return nil
	}
	sc.compact = nil
	sc.lock.Close()
	am.c, am.read = nil, 0
	return fmt.Errorf("%w: %v", errMemBroken, sc.broken)
}

// feed logs in sc the steps written to the subagent's log since it was
// last fed: whole entries only, each ending in a blank line (one cut short
// by a crash goes in once the next ends it). am.mu held, so no step is
// being written.
func (m *agentMems) feed(name string, am *agentMem, sc *Chat) error {
	f, err := os.Open(filepath.Join(m.main.dir, "agents", name+".log"))
	if err != nil {
		return err
	}
	defer f.Close()
	if am.read > 0 { // a log's first read needs no seek, so it may be a pipe, as in a test
		if _, err := f.Seek(am.read, io.SeekStart); err != nil {
			return err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if j := bytes.LastIndex(b, []byte("\n\n")); j < 0 {
		b = nil
	} else {
		b = b[:j+2]
	}
	steps := lo.Filter(parseSteps(b), func(s step, _ int) bool { return s.Kind != "thinking" })
	msg := func(s step) Msg {
		m := Msg{Kind: s.Kind, Text: s.Text, Ref: s.Ref}
		if s.At > 0 {
			m.Date = time.UnixMilli(s.At)
		}
		return m
	}
	var ms []Msg
	if am.read == 0 { // opened: skip what the chat holds, and end a step a crash cut short in it
		sc.mu.Lock()
		held := lo.CountBy(sc.msgs, func(m Msg) bool { return m.Part == 0 })
		if held > 0 && held <= len(steps) {
			parts := splitText(steps[held-1].Text, clipMax) // as logMsgs split it
			for j := sc.msgs[len(sc.msgs)-1].Part + 1; j < len(parts); j++ {
				m := msg(steps[held-1])
				m.Text, m.Part = parts[j], j
				ms = append(ms, m)
			}
		}
		sc.mu.Unlock()
		steps = steps[min(held, len(steps)):]
	}
	am.read += int64(len(b))
	ms = append(ms, lo.Map(steps, func(s step, _ int) Msg { return msg(s) })...)
	if len(ms) > 0 {
		sc.logMsgs(ms)
	}
	return nil
}

// closeIdle closes, past agentOpenMax open, the least used chats with
// nothing to build, but keep's. A closed one is opened again from its
// files when next needed. It holds agentMems.mu over closing their files.
func (m *agentMems) closeIdle(keep string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.open) <= agentOpenMax {
		return
	}
	type entry struct {
		name string
		am   *agentMem
	}
	var es []entry
	for name, am := range m.open {
		if name != keep {
			es = append(es, entry{name, am})
		}
	}
	slices.SortFunc(es, func(a, b entry) int { return cmp.Compare(a.am.used.Load(), b.am.used.Load()) })
	for _, e := range es {
		if len(m.open) <= agentOpenMax {
			return
		}
		if !e.am.mu.TryLock() { // being fed: in use. Only tried: see the lock order on agentMems
			continue
		}
		idle := true
		if sc := e.am.c; sc != nil {
			if !sc.mu.TryLock() { // writing a line to disk: in use, and agentMems.mu is held
				idle = false
			} else {
				if idle = sc.idle(); idle {
					sc.compact = nil
					sc.lock.Close()
				}
				sc.mu.Unlock()
			}
		}
		if idle {
			e.am.c, e.am.gone = nil, true
			delete(m.open, e.name)
		}
		e.am.mu.Unlock()
	}
}

// agentPlain is a subagent's whole chat in the chat dir, its steps' times
// and ids left out, and its thinking: what zoom("Name") gives with
// -agent-memory=false.
func agentPlain(dir, name string) string {
	name = agentName(name)
	b, err := os.ReadFile(filepath.Join(dir, "agents", name+".log"))
	if name == "" || err != nil {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	return string(plainSteps(b))
}

// chatRef ends a subagent's report: where its chat is.
func (a *Agent) chatRef(name string) string {
	if a.mems == nil {
		return fmt.Sprintf("Full chat: zoom(%q)", name)
	}
	return fmt.Sprintf("Its chat: zoom(%q)", name)
}

// ZoomAgent gives a subagent's chat as its view: lines that zoom opens as
// zoom("Name:id", n), within agentViewHi however long it ran.
func (a *Agent) ZoomAgent(name string) string {
	if a.mems == nil {
		return agentPlain(a.chat.dir, name)
	}
	name = agentName(name)
	if name == "" {
		return "No subagent []."
	}
	sc, err := a.mems.chat(name)
	if errors.Is(err, errMemBroken) {
		return memFailed(name, err) + " Its whole log:\n" + agentPlain(a.chat.dir, name)
	}
	if err != nil {
		return memFailed(name, err)
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	head := fmt.Sprintf("[%s]'s chat, %s, as lines: zoom(\"%s:id\", n) opens line id+n into two; zoom(\"%s:id\", 1) gives message id whole.",
		name, count(len(sc.msgs), "message"), name, name)
	if n := len(sc.ready0); n > 0 {
		head += fmt.Sprintf(" %s not summarized yet: their lines show the heads of their first and last messages.", count(n, "message"))
	}
	return head + "\n" + strings.Join(sc.callView(), "\n")
}

// ZoomAgentLine is zoom(id, n) in a subagent's chat.
func (a *Agent) ZoomAgentLine(name string, id, n int) (string, []string) {
	if a.mems == nil {
		return "Subagents' chats have no lines: opt-chat runs with -agent-memory=false; zoom(\"Name\") gives the whole log.", nil
	}
	name = agentName(name)
	if name == "" {
		return "No subagent [].", nil
	}
	sc, err := a.mems.chat(name)
	if err != nil {
		return memFailed(name, err), nil
	}
	text, _ := sc.Zoom(id, max(n, 1)) // a step has no images
	return text, nil
}

// errNoLog: the name has no log, so no subagent.
var errNoLog = errors.New("no log")

// errMemBroken: a write to the subagent's chat failed; it is closed, and
// opened again from its files when next needed.
var errMemBroken = errors.New("a write failed")

// memFailed is what zoom says when the subagent name's chat can't open.
func memFailed(name string, err error) string {
	if errors.Is(err, errNoLog) {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	if errors.Is(err, errMemBroken) {
		return fmt.Sprintf("[%s]'s memory failed: %v.", name, err)
	}
	return fmt.Sprintf("[%s]'s memory failed to open: %v.", name, err)
}

// sealed is b, a step, after a blank line if the log at path was cut
// short in one: the step then starts an entry of its own.
func sealed(path string, b []byte) []byte {
	if endsBlank(path) {
		return b
	}
	return append([]byte("\n\n"), b...)
}

// endsBlank is whether the log at path is empty or ends in a blank line,
// as every whole step does. One that can't be read is taken to: the write
// after fails on its own.
func endsBlank(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	b := make([]byte, 2)
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return true
	}
	if fi.Size() < 2 {
		return false
	}
	_, err = f.ReadAt(b, fi.Size()-2)
	return err != nil || string(b) == "\n\n"
}
