package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json/v2"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	w := &webServer{h: h, c: c, token: st.Token, hosts: []string{"127.0.0.1:" + port, "localhost:" + port},
		url: "http://127.0.0.1:" + port + "/?t=" + st.Token}
	go (&http.Server{Handler: w.routes(), ReadHeaderTimeout: 10 * time.Second}).Serve(ln)
	return w, nil
}

// listen opens addr, which must be on this machine, or the last run's port
// when free, or any.
func listen(addr string, last int) (net.Listener, error) {
	if addr != "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("-addr %s: the web chat listens on this machine only, as 127.0.0.1:port", addr)
		}
		return net.Listen("tcp", addr)
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
	mux.HandleFunc("GET /ws", w.ws)
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
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self' 'unsafe-inline'; "+
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

// file serves an attachment. Only images and PDFs show in the page; any
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
	T      string       `json:"t"` // send, edit, remove, pull, now, stop, history, paths
	Text   string       `json:"text"`
	Files  []attachment `json:"files"` // send: uploaded
	Paths  []string     `json:"paths"` // send: typed, to attach
	Now    bool         `json:"now"`   // send: and stop the turn underway for it
	ID     int          `json:"id"`
	Before int          `json:"before"`
	Seq    int          `json:"seq"`
}

type (
	historyReply struct {
		T    string `json:"t"` // history
		Msgs []Msg  `json:"msgs"`
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
	}
	sentReply struct {
		T   string `json:"t"` // sent
		Seq int    `json:"seq"`
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
		if out, ok := command(h.agent, req.Text); ok {
			return noticeUpdate{T: "notice", Level: "info", S: out}
		}
		ups, err := c.uploaded(req.Files)
		if err != nil {
			return failedReply{T: "failed", Seq: req.Seq, S: "Nothing sent: " + err.Error()}
		}
		typed, err := c.attachPaths(req.Paths)
		if err != nil {
			return failedReply{T: "failed", Seq: req.Seq, S: "Nothing sent: " + err.Error()}
		}
		if strings.TrimSpace(req.Text) == "" && len(ups)+len(typed) == 0 {
			return nil
		}
		m := c.compose(req.Text, append(ups, typed...))
		m.uploads = ups
		h.input(m)
		if req.Now {
			h.sendNow()
		}
		return sentReply{T: "sent", Seq: req.Seq}
	case "edit":
		if !h.edit(req.ID, req.Text) {
			return noticeUpdate{T: "notice", Level: "info", S: "That message has already gone to Opt."}
		}
	case "remove":
		h.pull(req.ID)
	case "pull":
		if m, ok := h.pull(req.ID); ok {
			return restoreReply{T: "restore", Text: m.text, Files: m.uploads}
		}
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
		return historyReply{T: "history", Msgs: c.msgs[max(0, before-page):before]}
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

func openBrowser(u string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	if err := exec.Command(cmd, u).Start(); err != nil {
		warn("open the address above in a browser: %v", err)
	}
}
