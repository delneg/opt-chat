package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The web chat: a page served on 127.0.0.1 only, behind a secret token,
// that gets the chat's updates over a WebSocket and sends the user's
// messages back on it. The token and port are kept in <dir>/web.json, so
// that a page left open reconnects after a restart.

//go:embed web
var webFiles embed.FS

const page = 200 // messages a page gets at once

type webServer struct {
	h     *harness
	c     *Chat
	token string
	hosts []string // the Host headers it answers: 127.0.0.1:port, localhost:port
	url   string

	speech *transcriber // voice messages' words; nil: off

	mu   sync.Mutex
	keys map[string]bool // the last sends' keys: a page sends again what a dropped connection may have lost
	seen []string
}

const keysKept = 1000

// firstSend reports whether a send with this key is new, and notes it.
// forget lets a send with key come again: what it carried was not sent.
func (w *webServer) forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.keys, key)
}

func (w *webServer) firstSend(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.keys[key] {
		return false
	}
	w.keys[key] = true
	w.seen = append(w.seen, key)
	if len(w.seen) > keysKept {
		delete(w.keys, w.seen[0])
		w.seen = w.seen[1:]
	}
	return true
}

type webState struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func serveWeb(h *harness, addr string) (*webServer, error) {
	c := h.agent.chat
	statePath := filepath.Join(c.dir, "web.json")
	var st webState
	if b, err := os.ReadFile(statePath); err == nil {
		json.Unmarshal(b, &st)
	}
	if len(st.Token) < 20 {
		st.Token = rand.Text()
	}
	ln, err := listen(addr, st.Port)
	if err != nil {
		return nil, err
	}
	st.Port = ln.Addr().(*net.TCPAddr).Port
	b, _ := json.Marshal(st)
	if err := os.WriteFile(statePath, b, 0o600); err != nil {
		return nil, err
	}
	port := strconv.Itoa(st.Port)
	w := &webServer{h: h, c: c, token: st.Token, hosts: []string{"127.0.0.1:" + port, "localhost:" + port}, keys: map[string]bool{},
		url: "http://127.0.0.1:" + port + "/?t=" + st.Token, speech: newTranscriber()}
	go (&http.Server{Handler: w.routes(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	return w, nil
}

// listen opens addr, which must be on this machine, or the last run's port
// when free, or any.
func listen(addr string, last int) (net.Listener, error) {
	if addr != "" {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if host != "127.0.0.1" && host != "localhost" { // the names guard answers to
			return nil, fmt.Errorf("-addr %s: the web chat listens on this machine only, as 127.0.0.1:port", addr)
		}
		return net.Listen("tcp", "127.0.0.1:"+port)
	}
	if last > 0 {
		if ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(last)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func (w *webServer) routes() http.Handler {
	static, _ := fs.Sub(webFiles, "web")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(rw, r, static, "index.html")
	})
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(rw, r)
	})))
	mux.HandleFunc("GET /files/{name}", w.file)
	mux.HandleFunc("POST /upload", w.upload)
	mux.HandleFunc("GET /voice", w.voice)
	mux.HandleFunc("POST /voice", w.voice)
	mux.HandleFunc("GET /ws", w.ws)
	mux.HandleFunc("GET /tree", w.tree)
	return w.guard(mux)
}

// guard lets in only requests to this machine's name for it, with the
// token: from the cookie, or from the address printed in the terminal,
// which sets the cookie. Pages from elsewhere get no answer they can read,
// and can't post or connect.
func (w *webServer) guard(next http.Handler) http.Handler {
	cookie := "opt-chat-" + strings.TrimPrefix(w.hosts[0], "127.0.0.1:")
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hd := rw.Header()
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		// No inline styles: what a reply holds can never restyle the page.
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self'; "+
			"connect-src 'self' ws://"+r.Host+"; frame-ancestors 'none'; object-src 'none'; base-uri 'none'; form-action 'none'")
		if !w.ownHost(r.Host) {
			http.Error(rw, "unknown host", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !w.ownOrigin(o) {
			http.Error(rw, "cross-origin request", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); t != "" && w.same(t) {
			http.SetCookie(rw, &http.Cookie{Name: cookie, Value: w.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
			q := r.URL.Query()
			q.Del("t")
			r.URL.RawQuery = q.Encode()
			http.Redirect(rw, r, r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		if k, err := r.Cookie(cookie); err != nil || !w.same(k.Value) {
			rw.Header().Set("Content-Type", "text/html; charset=utf-8")
			hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
			rw.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(rw, `<!doctype html><title>opt-chat</title><body style="font:15px ui-monospace,monospace;margin:3em;color:#888">`+
				`Open the address opt-chat printed in the terminal: it carries the key to this chat.`)
			return
		}
		next.ServeHTTP(rw, r)
	})
}

func (w *webServer) same(t string) bool {
	return subtle.ConstantTimeCompare([]byte(t), []byte(w.token)) == 1
}

func (w *webServer) ownHost(host string) bool {
	for _, h := range w.hosts {
		if host == h {
			return true
		}
	}
	return false
}

func (w *webServer) ownOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && w.ownHost(u.Host)
}

// file serves an attachment. Only images, PDFs and audio show in the page; any
// other file downloads, and nothing served from files/ runs a script.
func (w *webServer) file(rw http.ResponseWriter, r *http.Request) {
	name := "files/" + r.PathValue("name")
	if !safeName.MatchString(name) {
		http.NotFound(rw, r)
		return
	}
	hd := rw.Header()
	hd.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'")
	hd.Set("Cache-Control", "private, max-age=31536000, immutable") // named by content
	switch ext := strings.TrimPrefix(filepath.Ext(name), "."); ext {
	case "png", "jpeg", "jpg", "gif", "webp":
		hd.Set("Content-Type", "image/"+strings.Replace(ext, "jpg", "jpeg", 1))
	case "pdf":
		hd.Set("Content-Type", "application/pdf")
	case "webm", "ogg", "oga", "opus", "m4a", "mp4", "mp3", "wav", "flac": // a voice message plays in the page
		hd.Set("Content-Type", map[string]string{"oga": "audio/ogg", "opus": "audio/ogg", "m4a": "audio/mp4", "mp3": "audio/mpeg"}[ext])
		if hd.Get("Content-Type") == "" {
			hd.Set("Content-Type", "audio/"+ext)
		}
	default:
		hd.Set("Content-Type", "application/octet-stream")
		hd.Set("Content-Disposition", "attachment")
	}
	f, err := os.Open(filepath.Join(w.c.dir, name))
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		http.NotFound(rw, r)
		return
	}
	http.ServeContent(rw, r, "", fi.ModTime(), f)
}

// upload saves a file a page sends, its name in X-Name, and answers with
// the attachment to send with a message.
func (w *webServer) upload(rw http.ResponseWriter, r *http.Request) {
	name, err := url.PathUnescape(r.Header.Get("X-Name"))
	if err != nil || name == "" {
		name = "file"
	}
	if r.ContentLength > uploadMax { // said up front: refused before a byte is written
		http.Error(rw, fmt.Sprintf("%s is over %s", name, humanSize(uploadMax)), http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, uploadMax)
	at, err := w.c.save(r.Body, name)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	json.MarshalWrite(rw, at)
}

// request is what a page sends on its WebSocket.
type request struct {
	T      string       `json:"t"` // send, tellAgent, edit, remove, pull, now, stop, history, find, around, newer, paths, memview, zoom, steps, agentContext, ...
	Text   string       `json:"text"`
	Files  []attachment `json:"files"`  // send: uploaded; tellAgent takes the fields of send it uses, as send means them
	Paths  []string     `json:"paths"`  // send: typed, to attach; null: not looked for yet
	Skip   []string     `json:"skip"`   // send: typed, not to attach
	Chosen []string     `json:"chosen"` // send: typed and hidden, to attach
	Now    bool         `json:"now"`    // send: and stop the turn underway for it; tellAgent: and interrupt it at work for it
	ID     int          `json:"id"`     // around: the message; newer: the page's last
	N      int          `json:"n"`      // zoom: line id+n
	Before int          `json:"before"`
	Seq    int          `json:"seq"`
	Key    string       `json:"key"`    // send: the same on a send again after a reconnect
	Model  string       `json:"model"`  // model: the turns' model
	Effort string       `json:"effort"` // model: their effort
	Save   bool         `json:"save"`   // model: for later runs too
	Mode   string       `json:"mode"`   // mode: the permission mode
	Allow  bool         `json:"allow"`  // answer: yes
	Always bool         `json:"always"` // answer: yes, and don't ask again
	Action string       `json:"action"` // mcp: on, off, agents (for subagents only), reconnect, signin; skills: set; plugins: on, off; empty: list
	Name   string       `json:"name"`   // mcp, skills: which; steps, tellAgent, agentContext, compactAgent: the subagent's
	State  string       `json:"state"`  // skills: on, off, name-only, user-invocable-only
	Mine   bool         `json:"mine"`   // find: the user's own messages alone
}

type (
	historyReply struct {
		T      string  `json:"t"` // history
		Msgs   []Msg   `json:"msgs"`
		Asides []Aside `json:"asides"`
	}
	pathsReply struct {
		T     string      `json:"t"` // paths
		Seq   int         `json:"seq"`
		Found []foundPath `json:"found"`
	}
	foundPath struct {
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	restoreReply struct {
		T     string       `json:"t"` // restore: queued messages back to the input
		Text  string       `json:"text"`
		Files []attachment `json:"files"`
		Note  string       `json:"note,omitzero"` // why, when it is not plain
	}
	sentReply struct {
		T   string `json:"t"` // sent
		Seq int    `json:"seq"`
	}
	modelsReply struct {
		T      string      `json:"t"` // models: Claude Code's, for the picker
		Models []modelInfo `json:"models"`
		Error  string      `json:"error,omitzero"`
	}
	mcpReply struct {
		T       string      `json:"t"` // mcp: the user's MCP servers
		Servers []mcpServer `json:"servers"`
		SignIn  string      `json:"signIn,omitzero"` // the page to open to sign in to Name
		Name    string      `json:"name,omitzero"`
		Action  string      `json:"action,omitzero"` // the action answered; empty: a list asked for
		Error   string      `json:"error,omitzero"`
		Window  int         `json:"window,omitzero"` // the context window the servers' tokens are a share of; 0: not counted
	}
	filesReply struct {
		T     string           `json:"t"` // files: the @ menu's
		Seq   int              `json:"seq"`
		Items []fileSuggestion `json:"items"`
	}
	pluginsReply struct {
		T       string   `json:"t"` // plugins: the user's plugins
		Plugins []plugin `json:"plugins"`
		Action  string   `json:"action,omitzero"`
		Error   string   `json:"error,omitzero"`
	}
	skillsReply struct {
		T      string  `json:"t"` // skills: the user's skills
		Skills []skill `json:"skills"`
		Action string  `json:"action,omitzero"`
		Error  string  `json:"error,omitzero"`
	}
	failedReply struct {
		T   string `json:"t"` // failed: not sent, back to the input
		Seq int    `json:"seq"`
		S   string `json:"s"`
	}
)

func (w *webServer) ws(rw http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(rw, r, nil) // checks the Origin against the host
	if err != nil {
		return
	}
	conn.SetReadLimit(64 << 20)
	after, err := strconv.Atoi(r.URL.Query().Get("after"))
	if err != nil {
		after = -1
	}
	hub := w.c.hub
	cl := hub.join(w.c, after, page)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer hub.leave(cl)
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wctx, done := context.WithTimeout(ctx, 30*time.Second)
		defer done()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	go func() {
		defer cancel()
		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case b, ok := <-cl.out:
				if !ok {
					conn.Close(websocket.StatusTryAgainLater, "fell behind")
					return
				}
				wctx, done := context.WithTimeout(ctx, 30*time.Second)
				err := conn.Write(wctx, websocket.MessageText, b)
				done()
				if err != nil {
					return
				}
			case <-ping.C:
				pctx, done := context.WithTimeout(ctx, 30*time.Second)
				err := conn.Ping(pctx)
				done()
				if err != nil {
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			conn.CloseNow()
			return
		}
		var req request
		if err := json.Unmarshal(data, &req); err != nil {
			write(noticeUpdate{T: "notice", Level: "warn", S: "bad request: " + err.Error()})
			continue
		}
		if slow(w.h.agent, req) { // asks Claude Code: the page's sends go on meanwhile
			go func() {
				if reply := w.serveSlow(ctx, req); reply != nil {
					write(reply)
				}
			}()
			continue
		}
		if reply := w.serve(req); reply != nil {
			if err := write(reply); err != nil {
				return
			}
		}
	}
}

// serve answers one request; nil: no answer for the page alone.
func (w *webServer) serve(req request) any {
	c, h := w.c, w.h
	switch req.T {
	case "send":
		if req.Key != "" && !w.firstSend(req.Key) {
			return sentReply{T: "sent", Seq: req.Seq} // got it before the connection dropped
		}
		if reply, ok := w.command(h.agent, req); ok {
			return reply
		}
		ups, atts, err := w.attachments(&req)
		if err != nil {
			return w.failed(req, err)
		}
		if strings.TrimSpace(req.Text) == "" && len(atts) == 0 {
			return sentReply{T: "sent", Seq: req.Seq} // nothing to send
		}
		m := c.compose(req.Text, atts)
		m.uploads = ups
		if req.Now {
			h.inputNow(m)
		} else {
			h.input(m)
		}
		return sentReply{T: "sent", Seq: req.Seq}
	case "tellAgent": // the user talks to a subagent, as to Opt: a send's text and files, to it
		if req.Key != "" && !w.firstSend(req.Key) {
			return sentReply{T: "sent", Seq: req.Seq}
		}
		_, atts, err := w.attachments(&req)
		if err != nil {
			return w.failed(req, err)
		}
		if strings.TrimSpace(req.Text) == "" && len(atts) == 0 {
			return sentReply{T: "sent", Seq: req.Seq}
		}
		if out, ok := h.agent.TellUser(req.Name, req.Text, atts, req.Now); !ok {
			return w.failed(req, errors.New(out))
		}
		c.hub.setStatus(currentStatus(c, h.agent))
		return sentReply{T: "sent", Seq: req.Seq}
	case "edit":
		if !h.edit(req.ID, req.Text) { // the edit is not lost: back to the input
			return restoreReply{T: "restore", Text: req.Text, Files: []attachment{}, Note: "That message had already gone to Opt: your edit is back in the input."}
		}
	case "remove":
		h.pull(req.ID)
	case "pull":
		if m, ok := h.pull(req.ID); ok {
			return restoreReply{T: "restore", Text: m.text, Files: m.uploads}
		}
	case "model":
		if err := h.agent.choose(req.Model, req.Effort, req.Save); err != nil {
			return noticeUpdate{T: "notice", Level: "warn", S: err.Error()}
		}
		c.hub.setStatus(currentStatus(c, h.agent))
		how := "until you quit"
		if req.Save {
			how = "from now on"
		}
		return noticeUpdate{T: "notice", Level: "info", S: fmt.Sprintf("Turns: %s, effort %s, %s.", req.Model, req.Effort, how)}
	case "mode":
		if err := h.agent.setMode(req.Mode); err != nil {
			return noticeUpdate{T: "notice", Level: "warn", S: err.Error()}
		}
		c.hub.setStatus(currentStatus(c, h.agent))
	case "answer":
		h.agent.answer(req.ID, req.Allow, req.Always) // gone: answered elsewhere, or stopped
	case "now":
		h.sendNow()
	case "stop":
		back, _ := h.stop()
		if len(back) > 0 {
			var texts []string
			var files []attachment
			for _, m := range back {
				texts = append(texts, m.text)
				files = append(files, m.uploads...)
			}
			return restoreReply{T: "restore", Text: strings.Join(texts, "\n\n"), Files: files}
		}
	case "history":
		c.mu.Lock()
		defer c.mu.Unlock()
		before := min(max(req.Before, 0), len(c.msgs))
		from := c.whole(max(0, before-page))
		return historyReply{T: "history", Msgs: c.msgs[from:before], Asides: c.asidesIn(from, before)}
	case "around": // a find's match far back: the log around it (find.go)
		return c.around(req.ID)
	case "newer": // and scrolling down from there, toward the end
		return c.newer(req.ID)
	case "memview": // the page shows the chat as Opt's view (tree.go)
		return memViewReply{T: "memview", Lines: c.memView()}
	case "zoom":
		return c.memZoom(req.ID, req.N)
	case "steps": // the agents rail opens one: its log, then each step as it comes
		name := agentName(req.Name)
		steps, cut, err := h.agent.steps(name)
		if err != nil {
			return noticeUpdate{T: "notice", Level: "warn", S: fmt.Sprintf("No log for [%s]: %v", name, err)}
		}
		return stepsReply{T: "steps", Name: name, Steps: steps, Cut: cut, Live: h.agent.liveOf(name)}
	case "compactAgent": // the page's compact: now at rest, at its next step at work
		out, ok := h.agent.CompactAgent(agentName(req.Name))
		c.hub.setStatus(currentStatus(c, h.agent))
		level := "info"
		if !ok {
			level = "warn"
		}
		return noticeUpdate{T: "notice", Level: level, S: out}
	case "stopAgent", "clearAgent": // the rail's stop, then clear
		if req.T == "stopAgent" {
			h.agent.StopAgent(agentName(req.Name))
		} else {
			h.agent.ClearAgent(agentName(req.Name))
		}
		c.hub.setStatus(currentStatus(c, h.agent))
	case "paths":
		found := []foundPath{}
		for _, p := range filePaths(req.Text) {
			if fi, err := os.Stat(p); err == nil {
				found = append(found, foundPath{Path: p, Name: filepath.Base(p), Size: fi.Size()})
			}
		}
		return pathsReply{T: "paths", Seq: req.Seq, Found: found}
	default:
		return noticeUpdate{T: "notice", Level: "warn", S: "unknown request " + strconv.Quote(req.T)}
	}
	return nil
}

// failed answers a send that sent nothing: sent again, it is tried again.
func (w *webServer) failed(req request, err error) any {
	w.forget(req.Key)
	return failedReply{T: "failed", Seq: req.Seq, S: "Nothing sent: " + err.Error()}
}

// attachments are a send's: those uploaded, and with them those its typed
// paths name.
func (w *webServer) attachments(req *request) (ups, all []attachment, err error) {
	c := w.c
	if ups, err = c.uploaded(req.Files); err != nil {
		return nil, nil, err
	}
	if req.Paths == nil { // sent before the page looked: the files it names, as the page would
		req.Paths = slices.DeleteFunc(filePaths(req.Text), func(p string) bool {
			return slices.Contains(req.Skip, p) || hidden(p) && !slices.Contains(req.Chosen, p)
		})
	}
	typed, err := c.attachPaths(req.Paths)
	if err != nil {
		return nil, nil, err
	}
	return ups, append(slices.Clip(ups), typed...), nil
}

// command runs a send that is a command, its answer going to every page;
// false: a message. One that failed comes back to the page's input.
func (w *webServer) command(a *Agent, req request) (any, bool) {
	if q, ok := btwQuestion(req.Text); ok && strings.TrimSpace(q) == "" && len(req.Files)+len(req.Paths) > 0 {
		return nil, false // /btw with its question attached, a voice message say: a side question all the same
	}
	out, ok, err := command(a, req.Text)
	switch {
	case err != nil:
		w.forget(req.Key)
		return failedReply{T: "failed", Seq: req.Seq, S: err.Error() + "."}, true
	case ok:
		w.c.hub.send(noticeUpdate{T: "notice", Level: "info", S: out})
		return sentReply{T: "sent", Seq: req.Seq}, true
	}
	return nil, false
}

// slow are the requests that ask Claude Code, which can take seconds, and
// those that read much: the page's sends go on meanwhile.
func slow(a *Agent, req request) bool {
	switch req.T {
	case "models", "mcp", "skills", "plugins", "files", "usage", "agentContext", "find": // agentContext reads a session file, which can be big; find, the whole log
		return true
	case "send":
		return asks(a, req.Text)
	}
	return false
}

func (w *webServer) serveSlow(ctx context.Context, req request) any {
	a := w.h.agent
	switch req.T {
	case "find": // the page's find: matches, newest first, a page at a time (find.go)
		r := w.c.Find(req.Text, req.Before, req.Mine)
		r.Seq = req.Seq
		return r
	case "agentContext": // the full-screen view's: what the subagent was given
		name := agentName(req.Name)
		if r, ok := a.agentContext(name); ok {
			return r
		}
		return noticeUpdate{T: "notice", Level: "warn", S: fmt.Sprintf("No subagent [%s].", name)}
	case "usage": // its answer goes in the bar
		if err := a.askUsage(ctx); err != nil {
			dim("usage: " + err.Error())
		}
		w.c.hub.setStatus(currentStatus(w.c, a))
		return nil
	case "models":
		ms, err := a.models(ctx)
		if err != nil {
			return modelsReply{T: "models", Models: []modelInfo{}, Error: err.Error()}
		}
		return modelsReply{T: "models", Models: ms}
	case "files":
		items := fileSuggestions(a.files, req.Text)
		if items == nil {
			items = []fileSuggestion{}
		}
		return filesReply{T: "files", Seq: req.Seq, Items: items}
	case "send": // /mcp, /skills or /plugins, or a /word that may be a skill's
		if reply, ok := w.command(a, req); ok {
			return reply
		}
		return w.serve(req) // a skill's, listed now: a message
	case "mcp":
		if !a.ownConfig {
			return mcpReply{T: "mcp", Servers: []mcpServer{}, Error: "Opt runs without your Claude Code setup (-claude-config=false)."}
		}
		var err error
		r := mcpReply{T: "mcp", Name: req.Name, Action: req.Action}
		switch req.Action {
		case "on", "off", "agents":
			err = a.setMCP(ctx, req.Name, req.Action)
		case "reconnect":
			err = a.mcpReconnect(ctx, req.Name)
		case "signin":
			r.SignIn, err = a.mcpSignIn(ctx, req.Name)
		}
		if err != nil {
			r.Error = err.Error()
		}
		if r.Servers, err = a.mcpServers(ctx); err != nil {
			r.Servers, r.Error = []mcpServer{}, cmp.Or(r.Error, err.Error())
		}
		r.Window = a.mcpWindowNow()
		return r
	case "plugins":
		if !a.ownConfig {
			return pluginsReply{T: "plugins", Plugins: []plugin{}, Error: "Opt runs without your Claude Code setup (-claude-config=false)."}
		}
		r := pluginsReply{T: "plugins", Action: req.Action}
		if req.Action == "on" || req.Action == "off" {
			if err := a.setPlugin(ctx, req.Name, req.Action == "on"); err != nil {
				r.Error = err.Error()
			}
		}
		ps, err := a.plugins(ctx)
		if err != nil {
			ps, r.Error = []plugin{}, cmp.Or(r.Error, err.Error())
		}
		r.Plugins = ps
		return r
	case "skills":
		if !a.ownConfig {
			return skillsReply{T: "skills", Skills: []skill{}, Error: "Opt runs without your Claude Code setup (-claude-config=false)."}
		}
		r := skillsReply{T: "skills", Action: req.Action}
		if req.Action == "set" {
			if err := a.setSkill(ctx, req.Name, req.State); err != nil {
				r.Error = err.Error()
			}
		}
		ss, err := a.skills(ctx)
		if err != nil {
			ss, r.Error = []skill{}, cmp.Or(r.Error, err.Error())
		}
		r.Skills = ss
		return r
	}
	return nil
}

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		warn("open the address above in a browser: %v", err)
	}
}
