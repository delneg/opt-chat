package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Agent runs `claude -p` for turns and compactions: Claude Code is the tool
// loop (Bash, Read, Edit, ...); this program only owns the memory.
type Agent struct {
	chat            *Chat
	mems            *agentMems // the subagents' chats, see agentmem.go; nil: -agent-memory=false
	system          string     // the prompt: with agentsMD's instructions, the head of every cached prefix
	agentsMD        string     // the user's instructions, read again when the file changes; "": none
	mcpConfig       string     // the chat's tools, for turns and compactions
	agentMCP        string     // the same tools, for subagents, which can't spawn or tell
	model, effort   string     // turns
	amodel, aeffort string     // subagents
	cmodel, ceffort string     // compactions
	cbare           bool       // memory summary calls on a different model from the turns get no tools
	perm, cwd       string
	tools           string
	claude          string            // the claude command: "" for the one on PATH
	optMCP          string            // Opt's own MCP servers' file, which calls with the user's setup load: see optmcp.go; "": none
	claudeJSON      string            // Claude Code's config, whose servers they keep out
	files           string            // where @ completes paths from
	ownConfig       bool              // turns and subagents get the user's Claude Code skills, plugins and MCP servers
	claudeMd        bool              // and the CLAUDE.md files of the folders they work in, never the user's own
	mcpOff          []string          // of those servers, the ones the user turned off here
	mcpAgents       []string          // and those for subagents only: see mcpOffLocked
	rest            time.Duration     // agentRest, when set
	skillsSet       map[string]string // the user's skills' states set here: on, off, ...
	pluginsSet      map[string]bool   // and plugins turned on or off here, by name@marketplace
	skillsListed    []skill           // the skills Claude Code listed last, in its own states
	agentWindow     int               // the user's window for subagents' compaction (/autocompact), in tokens; 0: none
	agentCap        int               // -agent-compact-max: no subagent's context outgrows it; 0: no cap
	recall          int               // messages a turn gets that share the user's words: see Chat.Recall; 0: none. Set once, at start
	resting         map[string]*rest  // subagents that have reported, still able to go on: see Tell
	mcpUp           []string          // the servers connected when the last call sent its view: see mcpSettle
	mcpSeen         []string          // and those listed then
	mcpCounted      mcpCounts         // what each server takes of the context, as last counted: see countMCP

	report     func(text string) // hands an agent's report to the chat
	autoResume bool              // a start resumes the subagents a quit, crash or reload cut off: see resume.go
	onReload   func()            // /reload's: see reload.go; nil: none
	wakes      *waker            // the wake tool's (-wake); nil: off
	resumes    *resumer          // resumes at a usage limit's reset (-resume-at-reset); nil: off
	resetGrace time.Duration     // after the reset, before they go out
	noEmail    string            // the plugin that drops the account's email from calls' context, by folder; "": none. See email.go

	life  context.Context // canceled at quit: every call stops
	stop  context.CancelFunc
	calls sync.WaitGroup // calls and subagents underway

	mu          sync.Mutex
	cutting     bool                     // a reload is stopping every call: none starts
	compactions int                      // since the last turn's report
	cusage      usage                    // their usage
	running     map[string]*proc         // agents by name; nil while starting or finished
	writing     map[string]chan struct{} // first messages being written to the cache
	cached      map[[32]byte]time.Time   // compactions' first messages in the cache, by hash: when last written or read
	modelList   []modelInfo              // Claude Code's, once asked
	limits      map[string]window        // the plan's usage limits, by name: see limits.go
	refused     limitInfo                // the last rate_limit_event that gave a status: see refusedLocked
	usageAsked  time.Time                // when get_usage was last asked
	planRaw     jsontext.Value           // and its answer, for /usage
	subs        map[string]*sub          // this run's subagents, as the page's rail shows them: see subs.go
	halts       map[string]*halt         // each subagent's call underway, at work or at rest, by name: see StopAgent
	held        map[string][]tell        // tells for a subagent in a loop, at work: sent as its turn ends; see loop.go
	subLive     map[string]*agentLive    // what each subagent at work is writing now: see agentDelta
	userTurn    map[string]bool          // a subagent's turn underway is the user's: it reports to no one; see tell
	recMu       sync.Mutex               // agents/<Name>.json: see resume.go
	live        map[*proc]struct{}       // turns and subagents underway: a mode switch reaches them
	asks        []*ask                   // tool calls waiting for the user's yes or no
	askSeq      int
	ctlSeq      int
	allowed     []string // rules the user allowed for good this run, as "Bash(npm test:*)"
	dirs        []string // and folders

	woken      bool      // the turn underway was started by wake-ups: it sets none
	instructed string    // system with agentsMD as last read
	agentsSeen fileStamp // and that file then; zero: absent

	ctlMu  sync.Mutex
	ctl    *control   // nil until asked, and once stopped
	setMu  sync.Mutex // settings.json's writes
	modeMu sync.Mutex // mode switches' writes to the calls
}

// writeOnce lets one call at a time write a prefix to the cache: a call
// whose first message another call is writing waits until that call's
// response begins, then reads the entry instead of paying to write it too
// (gist §3.3; compactions start up to 8 at once on one prefix). The first
// caller gets a release func to call then; the others get a no-op.
func (a *Agent) writeOnce(ctx context.Context, prefix string) (release func()) {
	a.mu.Lock()
	if ch, ok := a.writing[prefix]; ok {
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done(): // stopped meanwhile: no use waiting
		}
		return func() {}
	}
	ch := make(chan struct{})
	a.writing[prefix] = ch
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		delete(a.writing, prefix)
		a.mu.Unlock()
		close(ch)
	}
}

// cachedFor is how long a first message is taken to stay cached after it
// was last written or read: Claude Code's cache entries live 5 minutes.
const cachedFor = 4 * time.Minute

// inCache reports whether the cache holds a compaction's first message,
// waiting for a call that is writing it to begin its response, or to fail
// before it, which leaves nothing cached; a call that goes on to read it
// keeps it there (noteCached).
func (a *Agent) inCache(ctx context.Context, prefix string) bool {
	h := sha256.Sum256([]byte(prefix))
	a.mu.Lock()
	ch, writing := a.writing[prefix]
	a.mu.Unlock()
	if writing {
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
	a.mu.Lock()
	at, ok := a.cached[h]
	a.mu.Unlock()
	return ok && time.Since(at) < cachedFor
}

// noteCached notes that a compaction's first message was written or read
// just now.
func (a *Agent) noteCached(prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cached == nil {
		a.cached = map[[32]byte]time.Time{}
	}
	maps.DeleteFunc(a.cached, func(_ [32]byte, at time.Time) bool { return time.Since(at) >= cachedFor })
	a.cached[sha256.Sum256([]byte(prefix))] = time.Now()
}

func (a *Agent) takeCompactions() (int, usage) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n, u := a.compactions, a.cusage
	a.compactions, a.cusage = 0, usage{}
	return n, u
}

type block struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	Source   *source        `json:"source,omitzero"` // image
	Thinking string         `json:"thinking,omitempty"`
	ID       string         `json:"id,omitempty"` // tool_use
	Name     string         `json:"name,omitempty"`
	Input    jsontext.Value `json:"input,omitzero"`
	ToolID   string         `json:"tool_use_id,omitempty"` // tool_result
	Content  jsontext.Value `json:"content,omitzero"`      // tool_result: a string or blocks
	IsError  bool           `json:"is_error,omitzero"`
	ImageURL jsontext.Value `json:"image_url,omitzero"` // Codex's input_image, read by import

	file string // fileBlocks: the saved file it holds, files/...
}

type source struct {
	Type      string `json:"type"` // base64
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

func textBlock(s string) block { return block{Type: "text", Text: s} }

type usage struct {
	Input  int `json:"input_tokens"`
	Write  int `json:"cache_creation_input_tokens"`
	Read   int `json:"cache_read_input_tokens"`
	Output int `json:"output_tokens"`
}

func (u *usage) add(v usage) {
	u.Input += v.Input
	u.Write += v.Write
	u.Read += v.Read
	u.Output += v.Output
}

func (u usage) String() string {
	k := func(n int) string { return fmt.Sprintf("%.1fk", float64(n)/1000) }
	return fmt.Sprintf("cache read %s, write %s, in %s, out %s", k(u.Read), k(u.Write), k(u.Input), k(u.Output))
}

type event struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	Parent  *string `json:"parent_tool_use_id"`
	Message struct {
		Content jsontext.Value `json:"content"`
		Usage   usage          `json:"usage"` // assistant: its request's
	} `json:"message"`
	Result  string         `json:"result"`
	IsError bool           `json:"is_error"`
	Usage   usage          `json:"usage"`
	Event   jsontext.Value `json:"event"`                  // stream_event: one of the API's own
	Think   int            `json:"estimated_tokens_delta"` // system thinking_tokens
	Compact struct {
		Trigger string `json:"trigger"` // manual: a /compact; auto: past its window, mid-turn
		Pre     int    `json:"pre_tokens"`
		Post    int    `json:"post_tokens"`
	} `json:"compact_metadata"` // system compact_boundary: Claude Code compacted the call's context
	Status string `json:"status"` // system status: compacting, or null once done

	RequestID string          `json:"request_id"` // control_request: Claude Code asks
	Request   jsontext.Value  `json:"request"`
	Response  controlResponse `json:"response"` // control_response: Claude Code answers

	Limits limitInfo `json:"rate_limit_info"` // rate_limit_event

	// result: the call's spend so far, at list prices; a resumed session's
	// starts from its last run's (see spent)
	Cost   float64               `json:"total_cost_usd"`
	APIms  float64               `json:"duration_api_ms"`
	Turns  int                   `json:"num_turns"` // requests for this result
	Models map[string]modelSpend `json:"modelUsage"`
}

// streamEvent is the API's streaming event inside a stream_event, which a
// turn's call sends as it goes (--include-partial-messages).
type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
		JSON     string `json:"partial_json"` // a tool call's input, as written
	} `json:"delta"`
	Message struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	Usage usage `json:"usage"`
}

// failure is what a result that is an error says, never empty.
func (e event) failure() string { return cmp.Or(e.Result, e.Subtype, "error") }

func (e event) blocks() []block {
	var bs []block
	if err := json.Unmarshal(e.Message.Content, &bs); err != nil {
		var s string
		if json.Unmarshal(e.Message.Content, &s) == nil {
			bs = []block{{Type: "text", Text: s}}
		}
	}
	return bs
}

// proc is one `claude -p` process speaking stream-json both ways.
type proc struct {
	prefix string          // what its cache prefix depends on besides the messages
	ctx    context.Context // done once the call is stopped
	cmd    *exec.Cmd
	mu     sync.Mutex
	stdin  io.WriteCloser // nil once closed
	out    *bufio.Scanner
	stderr strings.Builder // read only once the process has ended
	ended  sync.Once
	err    error
	done   func()

	declined  atomic.Bool // the user said no to a tool call, which stops the call
	who       string      // a subagent's name; empty for the turn
	deny      []string    // the MCP servers kept out of it: see ownSettings
	onRequest func(event) // takes Claude Code's control requests; nil: refused
	onLimits  func(limitInfo)
	onResult  func(event)
	spent     spent // the spend its results told so far: see noteSpend

	callMu   sync.Mutex // not mu: a write to the call can wait on its reader
	callSeq  int
	calls    map[string]chan controlReply // our control requests, waiting for answers
	callsEnd error                        // set once its output has ended
}

type role int

const (
	roleCompact role = iota
	roleTurn
	roleAgent
	roleBtw     // a side question: a turn's call that changes nothing (see btw.go)
	roleControl // takes control requests only: see control
)

// noTools blocks every tool call of a compaction. Its tools stay listed, the
// same as a turn's, so that on one model both read one cache entry (gist §4);
// with the user's own setup, a turn's add Skill and ToolSearch.
// ownSettings are a call's settings over the user's own: never their hooks
// or Claude Code's auto-compaction, but for a subagent's window (see
// start), nor their own CLAUDE.md when repos' ones load; their skills and
// plugins on or off as set here; deny's MCP servers kept out of the call.
// Claude Code's mcp_toggle would keep a server off in the user's own
// config for the folder (~/.claude.json), for every later call and their
// own sessions there; deniedMcpServers holds for this call alone. Under a.mu.
func (a *Agent) ownSettings(window int, deny []string) string {
	var excludes []string
	if a.claudeMd {
		excludes = []string{filepath.Join(claudeConfigDir(), "CLAUDE.md")}
	}
	type server struct {
		Name string `json:"serverName"`
	}
	var denied []server
	for _, name := range deny {
		denied = append(denied, server{name})
	}
	b, _ := json.Marshal(struct {
		DisableAllHooks    bool              `json:"disableAllHooks"`
		AutoCompactEnabled bool              `json:"autoCompactEnabled"`
		AutoCompactWindow  int               `json:"autoCompactWindow,omitzero"`
		ClaudeMdExcludes   []string          `json:"claudeMdExcludes,omitzero"`
		SkillOverrides     map[string]string `json:"skillOverrides,omitzero"`
		EnabledPlugins     map[string]bool   `json:"enabledPlugins,omitzero"`
		DeniedMcpServers   []server          `json:"deniedMcpServers,omitzero"`
	}{true, window > 0, ccWindow(window), excludes, a.skillsSet, a.pluginsSet, denied}, json.Deterministic(true))
	return string(b)
}

// compactSettings are the settings of a call without the user's setup:
// Claude Code's auto-compaction off, or on for a subagent's window.
func compactSettings(window int) string {
	if window == 0 {
		return `{"autoCompactEnabled":false}`
	}
	return fmt.Sprintf(`{"autoCompactEnabled":true,"autoCompactWindow":%d}`, ccWindow(window))
}

// ccBuffer is how far under its autoCompactWindow Claude Code compacts: it
// keeps room for a reply (the model's output limit, at most 20k tokens)
// and a 13k buffer. Measured on Claude Code 2.1.304: a window of 100k
// compacted at 70k, its threshold 67k.
const ccBuffer = 33_000

// ccWindow is the autoCompactWindow that compacts a context once it passes
// window, within Claude Code's 1M; 0 for none.
func ccWindow(window int) int {
	if window == 0 {
		return 0
	}
	return min(window+ccBuffer, 1_000_000)
}

// fileStamp tells a file's versions apart, as a build tool does.
type fileStamp struct {
	mod  int64 // mtime, in ns
	size int64
}

// systemNow is the system prompt with the user's instructions in agentsMD
// as the file is now. A change rewrites the cache once, for every role.
// Under a.mu.
func (a *Agent) systemNow() string {
	if a.agentsMD == "" {
		return a.system
	}
	var st fileStamp
	if fi, err := os.Stat(a.agentsMD); err == nil {
		st = fileStamp{fi.ModTime().UnixNano(), fi.Size()}
	}
	if a.instructed != "" && st == a.agentsSeen {
		return a.instructed
	}
	a.instructed, a.agentsSeen = a.system, st
	if b, err := os.ReadFile(a.agentsMD); err == nil && st != (fileStamp{}) {
		a.instructed += "\n# The user's instructions\n\n" + string(b)
	}
	return a.instructed
}

// withTools is the tools list with names added, each once.
func withTools(list string, names ...string) string {
	have := strings.Split(list, ",")
	for _, n := range names {
		if !slices.Contains(have, n) {
			have = append(have, n)
		}
	}
	return strings.Join(slices.DeleteFunc(have, func(s string) bool { return s == "" }), ",")
}

const noTools = `{"autoCompactEnabled":false,"hooks":{"PreToolUse":[{"matcher":"*","hooks":[{"type":"command","command":"echo 'Compactions call no tools.' >&2; exit 2"}]}]}}`

// start starts a call. Claude Code never compacts one, whatever the user's
// settings: the chat never ends, and its view never needs it. extra are a
// subagent's session flags.
func (a *Agent) start(ctx context.Context, model, effort string, r role, extra ...string) (*proc, error) {
	return a.startCall(ctx, model, effort, r, 0, "", extra...)
}

// startAgent starts a subagent's call. Past window, Claude Code compacts
// its context between two steps, never in one (see runAgent); 0: never.
func (a *Agent) startAgent(ctx context.Context, name, model, effort string, window int, extra ...string) (*proc, error) {
	return a.startCall(ctx, model, effort, roleAgent, window, name, extra...)
}

// claudeCmd is the claude command calls run.
func (a *Agent) claudeCmd() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return cmp.Or(a.claude, "claude")
}

// startCall starts a call; who is the subagent's name, "" for the chat's.
func (a *Agent) startCall(ctx context.Context, model, effort string, r role, window int, who string, extra ...string) (p *proc, err error) {
	if r != roleAgent {
		window = 0 // the chat's calls never compact
	}
	if !a.begin() {
		return nil, errors.New("opt-chat is quitting")
	}
	ctx, cancel := context.WithCancel(ctx)
	unhook := context.AfterFunc(a.life, cancel)
	done := func() { unhook(); cancel(); a.calls.Done() }
	defer func() {
		if err != nil {
			done()
		}
	}()
	if err := os.MkdirAll(a.cwd, 0o700); err != nil { // the system cleans its temp folder, where it is by default
		return nil, err
	}
	var ccDeny []string
	optMCP := ""
	if a.optMCP != "" && r != roleCompact {
		var ok bool
		if ccDeny, ok = ccDenied(a.claudeJSON, a.cwd, a.optMCP); ok {
			optMCP = a.optMCP
		}
	}
	a.mu.Lock()
	mcp, perm, allowed := a.mcpConfig, a.perm, append([]string{"mcp__opt__zoom", "mcp__opt__date", "mcp__opt__search", "mcp__opt__spawn", "mcp__opt__tell", "mcp__opt__wake"}, a.allowed...)
	dirs, own := slices.Clone(a.dirs), a.ownConfig && r != roleCompact
	var deny []string
	if r != roleControl { // the page's lists every server as it is
		deny = a.mcpOffLocked(who)
	}
	for _, name := range ccDeny {
		if !slices.Contains(deny, name) {
			deny = append(deny, name)
		}
	}
	settings, claudeMd := a.ownSettings(window, deny), a.claudeMd
	system := a.systemNow()
	bare := r == roleCompact && a.cbare && model != a.model
	a.mu.Unlock()
	switch r {
	case roleAgent:
		mcp = a.agentMCP
	case roleBtw: // the rest asks, and is refused (below); allow rules are no part of the cached prompt
		allowed = []string{"mcp__opt__zoom", "mcp__opt__date", "mcp__opt__search"}
	case roleCompact:
		// The mode only adds reminders after the system prompt. Auto mode,
		// where a model lacks it, adds a random "Exited Auto Mode: ask
		// clarifying questions" mid-call.
		perm = "default"
	}
	// Every role gets the same system prompt.
	tools := a.tools
	if bare {
		// Its tools only let it read the turns' prefix from the cache, which
		// is per model. Listed, they bring Claude Code's tool reminders, such
		// as Bash's commit attribution naming this call's model, and a
		// summary can take one for the chat's own words.
		tools = ""
	}
	if own {
		// ToolSearch: MCP servers' tools load as needed, not in every prompt.
		// Artifact publishes a page to the user's claude.ai account, as in
		// their own Claude Code; in a call like this one only with
		// CLAUDE_CODE_ARTIFACT (below).
		tools = withTools(tools, "Skill", "ToolSearch", "Artifact")
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", model, "--system-prompt", system, "--tools", tools, "--mcp-config", mcp}
	if optMCP != "" { // its servers replace the chat's own by the same name
		args = append(args, optMCP)
	}
	args = append(args, "--permission-mode", perm, "--allowedTools", strings.Join(allowed, ","))
	if r == roleAgent { // saved, so it can be resumed: see resume.go
		args = append(args, extra...)
	} else {
		args = append(args, "--no-session-persistence")
	}
	if own {
		// The user's skills, plugins, MCP servers and settings, as Claude Code
		// run in cwd has them; never their hooks, CLAUDE.md or memory (env,
		// settings), which would make Opt someone else. With -claude-md (on by
		// default), the CLAUDE.md files of the folders it works in.
		args = append(args, "--setting-sources", "user,project,local", "--settings", settings)
	} else {
		// Nothing of the user's Claude Code setup leaks in: no hooks, plugins, CLAUDE.md or MCP servers.
		args = append(args, "--setting-sources", "", "--strict-mcp-config", "--disable-slash-commands")
		if a.noEmail != "" { // under disableAllHooks, with the user's setup, it can't run
			args = append(args, "--plugin-dir", a.noEmail)
		}
		if r != roleCompact { // noTools says it for compactions
			args = append(args, "--settings", compactSettings(window))
		}
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if r != roleCompact { // the calls that ask: Claude Code puts its questions to us
		args = append(args, "--permission-prompt-tool", "stdio")
		for _, d := range dirs { // allowed for good this run
			args = append(args, "--add-dir", d)
		}
	}
	switch r {
	case roleCompact: // partial messages: its response's start lets the calls waiting on its prefix go (ack)
		args = append(args, "--settings", noTools, "--include-partial-messages")
	case roleTurn, roleAgent: // the reply streams to the pages as it is written
		args = append(args, "--include-partial-messages")
	}
	cmd := exec.CommandContext(ctx, a.claudeCmd(), args...)
	// Stopped gently, so Claude Code can stop what its tools started, and so
	// are they: its group (below).
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	// In a process group of its own: the terminal's Ctrl-C goes to the
	// foreground group, and would kill every call, compactions too. Calls
	// stop only through ctx.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = a.cwd
	cmd.Env = childEnv()
	if own {
		// Tool search always: left to decide, Claude Code defers MCP tools or
		// not by which servers have connected by each request, so the ack and
		// the turn after it disagreed, and the view was written again.
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "ENABLE_TOOL_SEARCH=true", "CLAUDE_CODE_ARTIFACT=1")
		if claudeMd { // not even from a Claude Code that started opt-chat
			cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=") })
		} else {
			cmd.Env = append(cmd.Env, "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1")
		}
	}
	// The system prompt and tools head the cache prefix: a change to
	// AGENTS.md leaves no entry cached for the old one.
	prefix := fmt.Sprintf("%s %s %x", model, effort, sha256.Sum256([]byte(system+"\x00"+tools)))
	p = &proc{prefix: prefix, ctx: ctx, cmd: cmd, done: done, onLimits: a.noteLimits, who: who, deny: deny}
	cmd.Stderr = &p.stderr
	if p.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p.out = bufio.NewScanner(stdout)
	p.out.Buffer(nil, 64<<20)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	children.Store(pid, a)
	if r == roleTurn || r == roleAgent {
		p.onRequest = func(e event) { a.request(p, e) }
		a.modeMu.Lock()
		a.mu.Lock()
		if a.live == nil {
			a.live = map[*proc]struct{}{}
		}
		a.live[p] = struct{}{}
		now := a.perm
		a.ctlSeq++
		id := fmt.Sprint("opt-mode-", a.ctlSeq)
		a.mu.Unlock()
		if now != perm { // switched as it started
			p.control(id, map[string]any{"subtype": "set_permission_mode", "mode": now})
		}
		a.modeMu.Unlock()
	}
	if r == roleBtw {
		p.onRequest = func(e event) {
			if e.Type == "control_request" {
				p.reply(e.RequestID, map[string]any{"behavior": "deny", "message": "A side question changes nothing: this call is refused. Answer with what you have."}, "")
			}
		}
	}
	if r != roleControl {
		p.onResult = func(e event) { a.noteSpend(p, r, e) }
		if slices.Contains(extra, "--resume") {
			p.spent = resumedSpend(p)
		}
	}
	p.done = func() {
		if p.ctx.Err() != nil { // stopped: nothing of it outlives it
			syscall.Kill(-pid, syscall.SIGKILL)
		}
		children.Delete(pid)
		a.mu.Lock()
		delete(a.live, p)
		a.mu.Unlock()
		done()
	}
	return p, nil
}

// children are the calls running, by pid, each with the Agent that
// started it. In process groups of their own, they would outlive opt-chat
// dying on an error: fatal stops them first.
var children sync.Map

// stopChildren stops the calls of owner, or all of them for nil.
func stopChildren(owner *Agent) {
	children.Range(func(pid, a any) bool {
		if owner == nil || a == owner {
			syscall.Kill(-pid.(int), syscall.SIGTERM) // its group: its tools too
		}
		return true
	})
}

// begin counts a call or subagent about to start; false once quitting.
func (a *Agent) begin() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.life.Err() != nil || a.cutting {
		return false
	}
	a.calls.Add(1)
	return true
}

// quit stops every call and subagent underway and waits until they have ended.
func (a *Agent) quit() {
	a.mu.Lock()
	a.stop()
	a.mu.Unlock()
	a.calls.Wait()
}

// childEnv is this process's environment minus the variables that tie a
// call to a Claude Code session that launched opt-chat. With them, calls
// got that session's token-budget reminder in some requests and not in
// others, so no two prompts matched, and inherited its effort.
func childEnv() []string { return childEnvOf(os.Environ()) }

// childEnvOf is childEnv, from the environment environ.
func childEnvOf(environ []string) []string {
	// Those of a Claude Code that started opt-chat, and the user's own
	// compaction window, which would beat a subagent's settings.
	inherited := []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN",
		"CLAUDE_CODE_EXECPATH", "CLAUDE_PID", "CLAUDE_EFFORT", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW", "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"}
	env := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(inherited, k)
	})
	// Long work goes to subagents, whose reports reach the chat. A shell left
	// running in the background would die with its call, its result unseen.
	// No server-side threads: with them, each request after the first sends
	// only what is new, and the server wrote the call's environment block
	// (skills, MCP servers: 20k tokens with the user's own setup) to the
	// cache again every turn; sent whole, the call reads it back.
	return append(env, "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1", "CLAUDE_CODE_TETHER_LIVE=0")
}

// send writes one user message; false once stdin is closed.
func (p *proc) send(content ...block) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return false
	}
	content = slices.DeleteFunc(slices.Clone(content), func(b block) bool { return b.Type == "text" && b.Text == "" })
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}})
	_, err := p.stdin.Write(append(b, '\n'))
	return err == nil
}

func (p *proc) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin != nil {
		p.stdin.Close()
		p.stdin = nil
	}
}

func (p *proc) next() (event, bool) {
	for p.out.Scan() {
		var e event
		if json.Unmarshal(p.out.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "control_response" { // to its request; returned too, for callNow
			p.answered(e.Response)
		}
		if e.Type == "rate_limit_event" {
			if p.onLimits != nil {
				p.onLimits(e.Limits)
			}
			continue
		}
		if e.Type == "result" && p.onResult != nil {
			p.onResult(e)
		}
		if e.Type == "control_request" || e.Type == "control_cancel_request" {
			switch {
			case p.onRequest != nil:
				p.onRequest(e)
			case e.Type == "control_request":
				p.reply(e.RequestID, nil, "not taken here")
			}
			continue
		}
		return e, true
	}
	p.endCalls()
	return event{}, false
}

// wait ends the call and gives its error; called as often as convenient.
func (p *proc) wait() error {
	p.ended.Do(func() {
		p.close()
		if err := p.cmd.Wait(); err != nil {
			p.err = err
			if last := lastLine(p.stderr.String()); last != "" {
				p.err = fmt.Errorf("%w: %s", err, last)
			}
		}
		p.done()
	})
	return p.err
}

// exited is the error for a call whose output ended before its result.
func (p *proc) exited() error { return fmt.Errorf("claude exited: %v", p.wait()) }

// How the view stays cached across calls. Claude Code marks for the cache
// the system prompt and the ends of the last two messages, and on models with
// mid-conversation system messages pins its environment block right after
// the first message. The API documents a 20-block lookback that should find
// a message's old end after it grew, but through Claude Code that never read
// back in our measurements (Fable 5.1, Opus 5.5, Sonnet 5.5, Haiku 5.5),
// while an exact repeat of a marked prefix always did. So a call opens with:
//
//	[view up to its last multiple of 64 lines] [env] ok [rest of the view] [input]
//
// The first message is byte-identical from call to call until 64 more lines
// arrive or a batch merges: each call reads it from the cache, and writes
// only the lines after it. The "ok" is the model's reply to it.
const checkpoint = 64

// viewGoesOn closes the first message, constant like the rest of it. Without
// it, a first message that ends on a request reads as one, and the model
// acts on it instead of answering "ok".
const viewGoesOn = `The view goes on in the next message, with the input: reply only "ok" to this one.`

// ccheckpoint is checkpoint for compactions. Their view is a few dozen lines,
// under one checkpoint as often as not, and those running at once share it
// to its end (compactInput): it changes as messages get built, so a finer
// step leaves less to write anew each time. Measured by TestCacheReplay.
const ccheckpoint = 8

// creuse is how many lines short of its own first message a compaction's
// first message may end and still be one cached already (compactView).
// Further back, the lines between them, written by every call that reuses
// it, cost more than writing the newer one once: one checkpoint and two did
// about as well in TestCacheReplay and on a real chat's import, three and
// more worse.
const creuse = ccheckpoint

// viewHere follows a first message sent with the rest of the view.
const viewHere = `The view goes on here, in this same message, with the input: answer the input, not with "ok".`

// viewMessages renders the view's two messages around input. A nil first
// message: fewer lines than one checkpoint.
func viewMessages(lines []string, input []block) (first, second []block) {
	return splitView(lines, len(lines), checkpoint, input)
}

// splitView is viewMessages with the first message ending at a multiple of
// every lines, within the first stable lines, and before the first line
// not summarized yet: once built, it would read otherwise, and the first
// message with it.
func splitView(lines []string, stable, every int, input []block) (first, second []block) {
	chat := func(ls []string) string {
		if len(ls) == 0 {
			return "<chat>\n</chat>"
		}
		return "<chat>\n" + strings.Join(ls, "\n") + "\n</chat>"
	}
	base := viewCut(lines, stable, every)
	if base > 0 {
		first = []block{textBlock(chat(lines[:base]) + "\n" + viewGoesOn)}
	}
	return first, append([]block{textBlock(chat(lines[base:]))}, input...)
}

// viewCut is where splitView ends the first message.
func viewCut(lines []string, stable, every int) int {
	if j := slices.IndexFunc(lines[:min(stable, len(lines))], func(l string) bool { return strings.HasSuffix(l, unbuilt) }); j >= 0 {
		stable = j
	}
	return min(stable, len(lines)) / every * every
}

// compactView splits a compaction's view as splitView does, unless the cache
// holds the first message, or one up to creuse lines shorter (inCache): then
// the call sends it with the rest in one message, and Claude Code's mark at
// that message's end reads it back. The API looks back for a cached prefix at
// the ends of the 20 blocks before a mark, while a first message sent alone,
// to be answered "ok" (ack), costs a request of its own, and one written
// anew costs the whole message again: during an import, when the view
// changes with nearly every line built, most first messages were written
// for one or two calls. TestCacheReplay measures it.
func (a *Agent) compactView(ctx context.Context, prefix string, lines []string, stable int, task []block) (first, rest []block) {
	first, rest = splitView(lines, stable, ccheckpoint, task)
	if first == nil || len(rest) > 16 { // the blocks after it, past the API's lookback
		return first, rest
	}
	cut := viewCut(lines, stable, ccheckpoint)
	for n, f, r := cut, first, rest; n > 0 && n >= cut-creuse; n -= ccheckpoint {
		if n < cut {
			f, r = splitView(lines, n, ccheckpoint, task)
		}
		if key := prefix + "\x00" + f[0].Text; a.inCache(ctx, key) {
			a.noteCached(key)
			return nil, slices.Concat(f, []block{textBlock(viewHere)}, r)
		}
	}
	return first, rest
}

// ack sends the view's first message and waits out the model's "ok",
// handing each other event to each (nil: drop them). Calls waiting to write
// the same first message go on as its response begins: the cache holds it
// from then on (noteCached), as UniiChat's single flight has it (AskAI.ts
// wait). A call that fails before lets them go with nothing cached.
func (a *Agent) ack(p *proc, first []block, u *usage, each func(event)) error {
	if first == nil {
		return nil
	}
	key := p.prefix + "\x00" + first[0].Text
	release := sync.OnceFunc(a.writeOnce(p.ctx, key))
	defer release()
	began := sync.OnceFunc(func() { a.noteCached(key); release() })
	p.send(first...)
	return awaitResult(p, u, func(e event) {
		if e.Type == "stream_event" || e.Type == "assistant" { // the response; not its own events, nor a mode switch's answer
			began()
		}
		if each != nil {
			each(e)
		}
	})
}

func awaitResult(p *proc, u *usage, each func(event)) error {
	for {
		e, ok := p.next()
		if !ok {
			return p.exited()
		}
		if e.Type != "result" {
			if each != nil {
				each(e)
			}
			continue
		}
		u.add(e.Usage)
		if e.IsError {
			return errors.New(e.failure())
		}
		return nil
	}
}

// Compact builds one tree line, enforcing the size with the ruler, the cut,
// and retries. The first stable lines of its context are the same for the
// compactions running meanwhile.
func (a *Agent) Compact(ctx context.Context, lines []string, stable int, task []block) (string, error) {
	p, err := a.start(ctx, a.cmodel, a.ceffort, roleCompact)
	if err != nil {
		return "", err
	}
	defer p.wait()
	var u usage
	defer func() {
		a.mu.Lock()
		a.compactions++
		a.cusage.add(u)
		a.mu.Unlock()
	}()
	first, rest := a.compactView(p.ctx, p.prefix, lines, stable, task)
	if err := a.ack(p, first, &u, nil); err != nil {
		return "", err
	}
	p.send(rest...)
	best, acks := "", 0
	for try := 1; ; {
		e, ok := p.next()
		if !ok && best != "" {
			return best, nil // the shortest line so far
		} else if !ok {
			return "", p.exited()
		}
		if e.Type != "result" {
			continue
		}
		u.add(e.Usage)
		line := strings.TrimSpace(echoedHead.ReplaceAllString(strings.TrimSpace(e.Result), ""))
		if (e.IsError || line == "") && best != "" {
			return best, nil
		} else if e.IsError {
			return "", fmt.Errorf("compaction: %s", e.failure())
		} else if line == "" {
			return "", errors.New("empty compaction")
		}
		// An "ok" is the reply the view's first message asks for, given to the
		// input too: no line. Ask once more, then fail, and let the line's
		// failures run their course rather than keep the "ok".
		if isAck(line) {
			if acks++; acks > 1 {
				if best != "" {
					return best, nil
				}
				return "", errors.New(`compaction answered "ok"`)
			}
			p.send(textBlock(`"ok" was the reply to the view's first message only. This message holds the` + "\n" +
				"<input>: write its one line now, as the task above says."))
			continue
		}
		if best == "" || len(line) < len(best) {
			best = line
		}
		// A few bytes over is fine: the view measures real sizes.
		if len(line) <= nodeMax || try >= 5 {
			return best, nil
		}
		try++
		p.send(textBlock(fmt.Sprintf("Too long: your line is %d bytes, over the 512-byte limit. Write\n"+
			"the whole line again for the same <input>, cutting just enough of the\n"+
			"least valuable items to fit before this cut:\n%s| ← LIMIT", len(line), head(line, nodeMax))))
	}
}

// echoedHead is a line's id+n| head, which a compaction sees on the lines it
// merges and may write back.
var echoedHead = regexp.MustCompile(`^\d+\+\d+\|\s*`)

// ackOnly matches a reply that is only "ok": the answer to the view's first
// message, never a line, since a message short enough to be "ok" is its own
// line with no model call.
var ackOnly = regexp.MustCompile(`(?i)^(ok(ay)?[.!]?\s*)+$`)

func isAck(s string) bool { return ackOnly.MatchString(strings.TrimSpace(s)) }

// message is one the chat hands a turn: the user's (kind user) or an agent's report (kind work).
type message struct {
	kind, text string
	inline     []placed     // attachments' marks with a place in the text: an edit keeps them
	extra      string       // and those with none, after it
	images     []string     // saved with Chat.saveFile: images and PDFs
	files      []string     // the attachments' names, as the queue shows them
	uploads    []attachment // those a page uploaded, which a stop hands back to it
	logged     bool         // by a call that died before it could answer
	at         int          // once logged: where in the log, or before
}

// full is m's text as the chat logs it: its own, with its attachments'
// marks in their places, and the rest after it. (An edit that took a
// placeholder out moves its mark to the end.)
func (m message) full() string {
	text, left := place(m.text, m.inline)
	extra := strings.Join(slices.DeleteFunc(append(left, m.extra), func(s string) bool { return s == "" }), "\n")
	switch {
	case extra == "":
		return text
	case text == "":
		return extra
	}
	return text + "\n\n" + extra
}

// content is m as a call gets it: its text, each image or PDF right after
// its mark in it.
func (c *Chat) content(m message) []block {
	return c.interleave(m.full(), m.images)
}

// interleave is text with each of the images and PDFs named (files/...)
// right after its attachment mark in it, so that "do this <img> not this
// <img>" reaches the model as written; those with no mark go at the end.
func (c *Chat) interleave(text string, names []string) []block {
	type at struct {
		end  int
		name string
	}
	var ats []at
	var rest []string
	from := map[string]int{} // a file attached twice: its second mark
	for _, name := range names {
		key := "/" + attr(filepath.ToSlash(name)) + `"` // the path's end: the chat's folder may have moved since
		i := strings.Index(text[from[key]:], key)
		if i < 0 {
			rest = append(rest, name)
			continue
		}
		i += from[key]
		end := strings.Index(text[i:], ">")
		if end < 0 {
			rest = append(rest, name)
			continue
		}
		from[key] = i + end + 1
		ats = append(ats, at{i + end + 1, name})
	}
	slices.SortStableFunc(ats, func(a, b at) int { return a.end - b.end })
	var out []block
	prev := 0
	for _, a := range ats {
		if s := text[prev:a.end]; s != "" {
			out = append(out, textBlock(s))
		}
		out = append(out, c.fileBlocks([]string{a.name})...)
		prev = a.end
	}
	if s := text[prev:]; s != "" || len(out) == 0 {
		out = append(out, textBlock(s))
	}
	return append(out, c.fileBlocks(rest)...)
}

// Turn is one fresh `claude -p` call: [system] [view] [message], logging
// each reply, tool call and result as it happens.
type Turn struct {
	agent    *Agent
	onTool   func() // called as each tool call starts: queued messages join the turn then
	mu       sync.Mutex
	open     bool      // taking messages: from its own until its result
	p        *proc     // set once the view and input are sent
	pending  []message // taken while the view was being sent
	tools    int       // tool calls started and not answered
	waiting  int       // the user's messages sent in while they run: read with their results
	cut      bool      // interrupted for them (hurry), until the cut ends in a result: their results are cut
	answered bool      // its last result went well: set by Run, read once it returns
}

const (
	refused = "The user doesn't want to proceed with this tool use." // how Claude Code answers a call cut short, as for a no
	cutEcho = "Interrupted: the user sent a message to read now."    // what the chat logs for one hurry cut
)

// inTool reports whether a tool call is running: a message sent now reaches
// the model with its result, as soon as it could.
func (t *Turn) inTool() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.open && t.p != nil && t.tools > 0
}

func (t *Turn) countTools(n int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tools = max(0, t.tools+n); t.tools == 0 && t.waiting > 0 { // read with the results
		t.setWaiting(0)
	}
}

// setWaiting tells the pages how many of the user's messages wait for the
// tool calls underway; t.mu held.
func (t *Turn) setWaiting(n int) {
	t.waiting = n
	t.agent.chat.hub.setTurn(func(lt *liveTurn) { lt.Waiting = n })
}

// hurry interrupts the tool calls underway, so that the model reads now the
// user's messages that wait for their results: Claude Code cuts them, ends
// the turn, and answers the messages in the same call. False when none
// wait.
func (t *Turn) hurry() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open || t.p == nil || t.tools == 0 || t.waiting == 0 {
		return false
	}
	if !t.cut && !t.p.control("opt-now", map[string]any{"subtype": "interrupt"}) {
		return false
	}
	t.cut = true
	return true
}

// hurried reports whether hurry cut the tool calls underway; done, the cut
// ends with the result it brings, so a later refusal or failure is real.
func (t *Turn) hurried(done bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cut := t.cut
	if done {
		t.cut = false
	}
	return cut
}

// Send hands a message to the running call, between its tool calls. When it
// returns false, a call that has just died may have logged m (m.logged).
func (t *Turn) Send(m *message) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open {
		return false
	}
	c := t.agent.chat
	c.mu.Lock()
	m.at = len(c.msgs)
	c.mu.Unlock()
	c.Log(m.kind, m.full(), m.images...)
	m.logged = true
	if t.p == nil {
		t.pending = append(t.pending, *m)
		return true
	}
	if !t.p.send(c.content(*m)...) {
		t.open = false
		return false
	}
	if m.kind == "user" && t.tools > 0 {
		t.setWaiting(t.waiting + 1)
	}
	return true
}

// detach closes the call to new messages, and returns those it took but
// never sent: later ones start a new turn.
func (t *Turn) detach() []message {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open = false
	if t.p != nil {
		t.p.close()
		t.p = nil
	}
	left := t.pending
	t.pending = nil
	return left
}

// atWork names the subagents at work, so that Opt knows, with no zoom,
// what is underway; nil when none is.
func (a *Agent) atWork() []block {
	a.mu.Lock()
	defer a.mu.Unlock()
	if names := slices.Sorted(maps.Keys(a.running)); len(names) > 0 {
		return []block{textBlock("[opt-chat] Subagents at work now: [" + strings.Join(names, "], [") + "].\n\n")}
	}
	return nil
}

func (t *Turn) Run(ctx context.Context, msgs []message) error {
	a, c := t.agent, t.agent.chat
	h := c.hub
	c.mu.Lock()
	lines := c.callView() // rendered before the new messages are logged
	partial, from := c.unsummarized(), len(c.msgs)
	c.mu.Unlock()
	h.setTurn(func(lt *liveTurn) {
		lt.Phase, lt.Partial, lt.From, lt.Backlog, lt.Written, lt.Tool, lt.Context = "reading", partial, from, 0, 0, "", 0
	})
	input := a.atWork()
	// Recall reads the log while the call starts. It looks before the
	// messages of this turn: one logged already (by a call that died) would
	// find itself.
	var asked []string // the user's words, a voice message's too
	before := from
	for _, m := range msgs {
		if m.kind == "user" {
			asked = append(asked, unframed(m.full()))
		}
		if m.logged {
			before = min(before, m.at)
		}
	}
	recalled, recallAt := make(chan string, 1), len(input)
	go func() { recalled <- c.Recall(strings.Join(asked, "\n"), before, a.recall) }()
	for _, m := range msgs {
		if !m.logged {
			c.Log(m.kind, m.full(), m.images...)
		}
		input = append(input, c.content(m)...)
	}
	t.mu.Lock()
	t.open = true
	t.mu.Unlock()
	a.mu.Lock()
	model, effort := a.model, a.effort // /model and /effort change them
	a.mu.Unlock()
	p, err := a.start(ctx, model, effort, roleTurn)
	if err != nil {
		return err
	}
	var vu, u usage
	acking := true
	var written strings.Builder   // the reply being written, until it is logged
	done, out, thought := 0, 0, 0 // output tokens: of the replies before, this one's, its thinking's estimate
	context := 0                  // the biggest request's input tokens
	phase := func(p, tool string) {
		h.setTurn(func(lt *liveTurn) { lt.Phase, lt.Tool, lt.Tokens = p, tool, done+max(out, thought) })
	}
	// Everything the model does is logged as it happens, even during the
	// ack, where only its "ok" is dropped.
	handle := func(e event) {
		if e.Parent != nil {
			return // a subagent's own steps stay out of the chat
		}
		switch e.Type {
		case "stream_event":
			var s streamEvent
			if acking || json.Unmarshal(e.Event, &s) != nil {
				return
			}
			switch s.Type {
			case "message_start":
				done, out, thought = done+out, s.Message.Usage.Output, 0
				in := s.Message.Usage.Input + s.Message.Usage.Read + s.Message.Usage.Write
				context = max(context, in)
				h.setTurn(func(lt *liveTurn) { lt.Context = in })
				phase("thinking", "")
			case "message_delta": // the message's end, as the tool it called starts: the phase stays
				out = s.Usage.Output
				h.setTurn(func(lt *liveTurn) { lt.Tokens = done + max(out, thought) })
			case "content_block_start":
				switch s.ContentBlock.Type {
				case "text":
					written.Reset()
					h.restart("text")
					phase("writing", "")
				case "tool_use": // its input streams, so the pages show what it will run as it is written
					h.restart("input")
					phase("tool", strings.TrimPrefix(s.ContentBlock.Name, "mcp__opt__"))
				case "thinking":
					h.restart("thinking")
					phase("thinking", "")
				}
			case "content_block_delta":
				switch s.Delta.Type {
				case "text_delta":
					written.WriteString(s.Delta.Text)
					h.delta("text", s.Delta.Text)
				case "thinking_delta":
					h.delta("thinking", s.Delta.Thinking)
				case "input_json_delta":
					h.delta("input", s.Delta.JSON)
				}
			}
		case "system":
			if e.Subtype == "thinking_tokens" && !acking {
				thought += e.Think
				phase("thinking", "")
			}
		case "assistant":
			for _, b := range e.blocks() {
				switch b.Type {
				case "thinking":
					if b.Thinking != "" && !acking {
						h.thought(b.Thinking)
					}
				case "text":
					if !acking {
						h.clearText()
						written.Reset()
						c.Log("opt", b.Text)
					}
				case "tool_use":
					h.clearInput()
					c.logRef("tool", toolCall(b), b.ID)
					t.countTools(1)
					phase("tool", strings.TrimPrefix(b.Name, "mcp__opt__"))
					if !acking && t.onTool != nil {
						t.onTool()
					}
				}
			}
		case "user":
			for _, b := range e.blocks() {
				if b.Type == "tool_result" {
					out, imgs := resultContent(b.Content)
					if strings.HasPrefix(out, refused) && t.hurried(false) {
						out = cutEcho
					}
					var names []string
					for _, img := range imgs {
						if name, err := c.saveImage(img); err == nil {
							names = append(names, name)
						} else {
							h.notice("warn", "echo image: %v", err)
						}
					}
					c.logRef("echo", clip(out, clipMax), b.ToolID, names...)
					t.countTools(-1)
				}
			}
			if !acking && !t.inTool() { // with every result in: thinking again
				phase("thinking", "")
			}
		}
	}
	a.mcpSettle(p)
	if r := <-recalled; r != "" {
		input = slices.Insert(input, recallAt, textBlock(r))
	}
	first, second := viewMessages(lines, input)
	if err := a.ack(p, first, &vu, handle); err != nil {
		p.wait()
		return err
	}
	acking = false
	phase("thinking", "")
	t.mu.Lock()
	p.send(second...)
	for _, m := range t.pending {
		p.send(c.content(m)...)
	}
	t.p, t.pending = p, nil
	t.mu.Unlock()
	failed := "" // what its result said went wrong
	for {
		e, ok := p.next()
		if !ok {
			break
		}
		if e.Type != "result" {
			handle(e)
			continue
		}
		u.add(e.Usage)
		t.answered = !e.IsError
		cut := e.Subtype == "error_during_execution" && t.hurried(true) // the hurry's: the messages' answer follows
		if !e.IsError {
			failed = "" // a later result went well: the turn did not fail
		} else if ctx.Err() == nil && !p.declined.Load() && !cut {
			failed = e.failure()
		}
		// Claude Code delivers a message sent mid-run into this same result,
		// so the call is done: close it to new messages and let it exit.
		t.detach()
	}
	t.detach()
	err = p.wait()
	switch {
	case p.declined.Load(): // the user said no, which ends the turn: no failure
		err = nil
	case failed != "": // said better than by its exit status
		err = errors.New(failed)
	case t.answered && ctx.Err() != nil: // stopped as it exited after its answer: no failure
		err = nil
	}
	if s := written.String(); s != "" { // stopped mid-reply: what it said stays in the chat
		h.clearText()
		c.Log("opt", s+" [interrupted]")
	}
	n, cu := a.takeCompactions()
	h.ended(turnEnd{View: vu, Turn: u, Context: context, Compactions: n, CUsage: cu})
	return err
}

// resultContent is a tool result's text, with a mark where each image was,
// and its images.
func resultContent(v jsontext.Value) (string, [][]byte) {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, nil
	}
	var bs []block
	json.Unmarshal(v, &bs)
	var parts []string
	var imgs [][]byte
	for _, b := range bs {
		switch {
		case b.Type == "text":
			parts = append(parts, b.Text)
		case b.Type == "image" && b.Source != nil && b.Source.Type == "base64":
			if img, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
				imgs = append(imgs, img)
			}
			parts = append(parts, "[image]")
		default:
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n"), imgs
}

// toolCall is a tool_use as the log keeps it: its name and JSON input, the
// chat's own tools by the names the prompt gives them.
func toolCall(b block) string {
	return strings.TrimPrefix(b.Name, "mcp__opt__") + " " + string(b.Input)
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = head(s, n) + "…"
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if _, after, ok := strings.CutLast(s, "\n"); ok {
		return after
	}
	return s
}

func dim(s string) { fmt.Fprintf(os.Stdout, "\x1b[2m%s\x1b[0m\n", s) }
