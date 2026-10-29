package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type webTest struct {
	t      *testing.T
	w      *webServer
	h      *harness
	c      *Chat
	base   string
	cookie string
}

func startWeb(t *testing.T) *webTest {
	c, _ := fakeChat(t, t.TempDir(), 100)
	record(c)
	h := newHarness(t, c)
	w, err := serveWeb(h, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var st webState
	b, _ := os.ReadFile(filepath.Join(c.dir, "web.json"))
	if err := json.Unmarshal(b, &st); err != nil || st.Token != w.token || st.Port == 0 {
		t.Fatalf("web.json %s: %v", b, err)
	}
	port := strconv.Itoa(st.Port)
	return &webTest{t: t, w: w, h: h, c: c, base: "http://127.0.0.1:" + port, cookie: "opt-chat-" + port + "=" + w.token}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (wt *webTest) do(method, path string, body io.Reader, hdr map[string]string) *http.Response {
	wt.t.Helper()
	req, _ := http.NewRequest(method, wt.base+path, body)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		wt.t.Fatal(err)
	}
	wt.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func pngBytes(w, h int) []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h)))
	return b.Bytes()
}

// Only this machine's names for the server, with the key, get in; the key
// in the address becomes a cookie; nothing from files/ runs a script.
func TestWebGuard(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ck := map[string]string{"Cookie": wt.cookie}
	for _, c := range []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"no key", "GET", "/", nil, 401},
		{"wrong key", "GET", "/?t=nope", nil, 401},
		{"wrong cookie", "GET", "/", map[string]string{"Cookie": "opt-chat-1=" + wt.w.token}, 401},
		{"key", "GET", "/", ck, 200},
		{"script", "GET", "/static/app.js", ck, 200},
		{"rebound name", "GET", "/", map[string]string{"Cookie": wt.cookie, "Host": "evil.example:" + strings.Split(wt.base, ":")[2]}, 403},
		{"cross-site post", "POST", "/upload", map[string]string{"Cookie": wt.cookie, "Origin": "http://evil.example"}, 403},
		{"no key upload", "POST", "/upload", nil, 401},
		{"no key socket", "GET", "/ws", nil, 401},
		{"escape files", "GET", "/files/..%2fweb.json", ck, 404},
		{"no such file", "GET", "/files/0123456789abcdef0123456789abcdef.png", ck, 404},
	} {
		if resp := wt.do(c.method, c.path, nil, c.hdr); resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	resp := wt.do("GET", "/?t="+wt.w.token, nil, nil)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" || !strings.Contains(resp.Header.Get("Set-Cookie"), wt.cookie) ||
		!strings.Contains(resp.Header.Get("Set-Cookie"), "HttpOnly") || !strings.Contains(resp.Header.Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("the key in the address: %d, %v", resp.StatusCode, resp.Header)
	}
	page := wt.do("GET", "/", nil, ck)
	if csp := page.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "script-src") || strings.Contains(csp, "unsafe-inline") {
		t.Fatalf("the page's CSP: %q", csp)
	}

	up := func(name string, body []byte) attachment {
		resp := wt.do("POST", "/upload", bytes.NewReader(body), map[string]string{"Cookie": wt.cookie, "X-Name": name, "Origin": wt.base})
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("upload %s: %d %s", name, resp.StatusCode, b)
		}
		var at attachment
		json.UnmarshalRead(resp.Body, &at)
		return at
	}
	img := up("shot%20one.png", pngBytes(10, 10))
	if img.Kind != "image" || img.Orig != "shot one.png" || !safeName.MatchString(img.Name) {
		t.Fatalf("uploaded image: %+v", img)
	}
	resp = wt.do("GET", "/"+img.Name, nil, ck)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("an image: %d %v", resp.StatusCode, resp.Header)
	}
	page2 := up("x.html", []byte("<script>alert(1)</script>"))
	resp = wt.do("GET", "/"+page2.Name, nil, ck)
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("an HTML file served as %v", resp.Header)
	}
}

type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func (wt *webTest) dial(after int, hdr http.Header) (*wsClient, error) {
	if hdr == nil {
		hdr = http.Header{}
	}
	hdr.Set("Cookie", wt.cookie)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, strings.Replace(wt.base, "http", "ws", 1)+"/ws?after="+strconv.Itoa(after), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	wt.t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: wt.t, conn: conn}, nil
}

func (ws *wsClient) send(v any) {
	b, _ := json.Marshal(v)
	if err := ws.conn.Write(context.Background(), websocket.MessageText, b); err != nil {
		ws.t.Fatal(err)
	}
}

// next reads updates until one satisfies ok, which it returns.
func (ws *wsClient) next(what string, ok func(map[string]any) bool) map[string]any {
	ws.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, b, err := ws.conn.Read(ctx)
		if err != nil {
			ws.t.Fatalf("waiting for %s: %v", what, err)
		}
		var u map[string]any
		json.Unmarshal(b, &u)
		if ok(u) {
			return u
		}
	}
}

func typ(t string) func(map[string]any) bool {
	return func(u map[string]any) bool { return u["t"] == t }
}

// A page's whole round: hello, a message with an upload and a typed path,
// the reply as it is logged, history, path lookups, commands, a failed send
// back to the input, and a reconnect that gets only what it missed.
func TestWebChat(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	if _, err := wt.dial(-1, http.Header{"Origin": {"http://evil.example"}}); err == nil {
		t.Fatal("a socket from another site")
	}
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := ws.next("hello", typ("hello")); u["total"].(float64) != 0 || u["reset"] != true {
		t.Fatalf("hello: %v", u)
	}
	resp := wt.do("POST", "/upload", bytes.NewReader(pngBytes(4, 4)), map[string]string{"Cookie": wt.cookie, "X-Name": "a.png"})
	var img attachment
	json.UnmarshalRead(resp.Body, &img)
	typed := filepath.Join(t.TempDir(), "notes.txt")
	os.WriteFile(typed, []byte("the answer is 42\n"), 0o600)

	ws.send(map[string]any{"t": "paths", "text": "see " + typed + " and /nope", "seq": 7})
	if u := ws.next("paths", typ("paths")); u["seq"].(float64) != 7 || len(u["found"].([]any)) != 1 {
		t.Fatalf("paths: %v", u)
	}
	img.At = "[Image #1]"
	ws.send(map[string]any{"t": "send", "text": "look [Image #1] then " + typed, "files": []attachment{img}, "paths": []string{typed}, "seq": 1})
	ws.next("sent", func(u map[string]any) bool { return u["t"] == "sent" && u["seq"].(float64) == 1 })
	user := ws.next("the user's message", func(u map[string]any) bool {
		return u["t"] == "msgs" && u["msgs"].([]any)[0].(map[string]any)["kind"] == "user"
	})["msgs"].([]any)[0].(map[string]any)
	text := user["text"].(string)
	// each attachment where it was put: the image for its placeholder, the
	// typed file after its path
	if !strings.HasPrefix(text, "look <attachment name=\"a.png\" path=") || !strings.Contains(text, "/> then "+typed+" <attachment name=\"notes.txt\"") ||
		!strings.HasSuffix(text, ">\nthe answer is 42\n</attachment>") || len(user["images"].([]any)) != 1 {
		t.Fatalf("the user's message: %v", user)
	}
	ws.next("the reply", func(u map[string]any) bool {
		if u["t"] != "msgs" {
			return false
		}
		m := u["msgs"].([]any)[0].(map[string]any)
		return m["kind"] == "opt" && strings.Contains(m["text"].(string), "with 1 images")
	})
	ws.next("the end", typ("end"))
	waitFor(t, "idle", wt.h.idle)

	ws.send(map[string]any{"t": "history", "before": 2})
	if u := ws.next("history", typ("history")); len(u["msgs"].([]any)) != 2 {
		t.Fatalf("history: %v", u)
	}
	ws.send(map[string]any{"t": "send", "text": "/effort high", "seq": 2})
	if u := ws.next("command", typ("notice")); u["s"] != "Turns: big, effort high, mode auto." {
		t.Fatalf("a command: %v", u)
	}
	ws.send(map[string]any{"t": "send", "text": "x", "files": []attachment{{Name: "files/../web.json"}}, "seq": 3})
	if u := ws.next("failed", typ("failed")); u["seq"].(float64) != 3 || !strings.HasPrefix(u["s"].(string), "Nothing sent") {
		t.Fatalf("a bad send: %v", u)
	}
	ws.send(map[string]any{"t": "stop"}) // idle: no answer, no harm
	ws.send(map[string]any{"t": "what"})
	if u := ws.next("unknown", typ("notice")); !strings.Contains(u["s"].(string), "unknown request") {
		t.Fatalf("unknown request: %v", u)
	}

	wt.c.mu.Lock()
	n := len(wt.c.msgs)
	wt.c.mu.Unlock()
	again, err := wt.dial(n-2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u := again.next("hello", typ("hello")); u["reset"] != false || len(u["msgs"].([]any)) != 1 {
		t.Fatalf("a reconnect: reset %v, %d msgs", u["reset"], len(u["msgs"].([]any)))
	}
}

// queueOf is a queue update holding just text.
func queueOf(text string) func(map[string]any) bool {
	return func(u map[string]any) bool {
		q, _ := u["queue"].([]any)
		return u["t"] == "queue" && len(q) == 1 && q[0].(map[string]any)["text"] == text
	}
}

// Stop from a page hands its queued messages back to that page.
func TestWebStopRestores(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "send", "text": "wait-for-stop", "seq": 1})
	waitFor(t, "the turn", func() bool { return hasLogged(wt.c, "opt: working") })
	ws.send(map[string]any{"t": "send", "text": "queued one", "seq": 2})
	ws.next("queue", queueOf("queued one"))
	ws.send(map[string]any{"t": "stop"})
	if u := ws.next("restore", typ("restore")); u["text"] != "queued one" {
		t.Fatalf("restore: %v", u)
	}
	waitFor(t, "idle", wt.h.idle)
	ws.send(map[string]any{"t": "send", "text": "wait-for-stop", "seq": 3})
	waitFor(t, "the turn", func() bool {
		return len(logged(wt.c)) > 3 && strings.HasSuffix(logged(wt.c)[len(logged(wt.c))-1], "working")
	})
	ws.send(map[string]any{"t": "send", "text": "pull me", "seq": 4})
	q := ws.next("queue", queueOf("pull me"))
	id := q["queue"].([]any)[0].(map[string]any)["id"]
	ws.send(map[string]any{"t": "pull", "id": id})
	if u := ws.next("restore", typ("restore")); u["text"] != "pull me" {
		t.Fatalf("pull: %v", u)
	}
	ws.send(map[string]any{"t": "stop"})
	waitFor(t, "idle", wt.h.idle)
}

// The page listens on 127.0.0.1 only, whatever -addr says.
func TestListen(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"0.0.0.0:0", "[::1]:0", "192.168.1.1:0", "example.com:0", "nope"} {
		if ln, err := listen(bad, 0); err == nil {
			ln.Close()
			t.Errorf("listen(%q) took it", bad)
		}
	}
	ln, err := listen("localhost:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ip := ln.Addr().(*net.TCPAddr).IP; !ip.Equal(net.IPv4(127, 0, 0, 1)) {
		t.Fatalf("localhost listened on %v", ip)
	}
}

// A send the page makes again after a reconnect, with the same key, is
// taken once.
func TestWebSendOnce(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	for seq := range 2 {
		ws.send(map[string]any{"t": "send", "text": "only once", "seq": seq, "key": "page:1"})
		ws.next("sent", func(u map[string]any) bool { return u["t"] == "sent" && u["seq"].(float64) == float64(seq) })
	}
	waitFor(t, "idle", wt.h.idle)
	if n := len(slices.DeleteFunc(logged(wt.c), func(s string) bool { return s != "user: only once" })); n != 1 {
		t.Fatalf("logged %d times", n)
	}
	// A send that failed fails again when sent again: it was never taken.
	for seq := 2; seq < 4; seq++ {
		ws.send(map[string]any{"t": "send", "text": "x", "files": []map[string]any{{"name": "files/../web.json"}}, "seq": seq, "key": "page:2"})
		ws.next("failed", func(u map[string]any) bool { return u["t"] == "failed" && u["seq"].(float64) == float64(seq) })
	}
}

// An upload that says it is too big is refused before it is read.
func TestUploadTooBig(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	req := httptest.NewRequest("POST", "/upload", strings.NewReader("x"))
	req.ContentLength = uploadMax + 1
	rec := httptest.NewRecorder()
	wt.w.upload(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", rec.Code)
	}
	if ents, _ := os.ReadDir(filepath.Join(wt.c.dir, "files")); len(ents) != 0 {
		t.Fatalf("files written: %v", ents)
	}
}

// A message sent before the page looked for the paths in it: the server
// looks, and leaves hidden files for the page to ask about.
func TestWebFindsPaths(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	dir := t.TempDir()
	plain, secret := filepath.Join(dir, "plain.txt"), filepath.Join(dir, ".secret")
	os.WriteFile(plain, []byte("PLAIN"), 0o600)
	os.WriteFile(secret, []byte("SECRET"), 0o600)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "send", "text": "see " + plain + " and " + secret, "seq": 1, "key": "page:1"})
	ws.next("sent", typ("sent"))
	waitFor(t, "idle", wt.h.idle)
	got := strings.Join(logged(wt.c), "\n")
	if !strings.Contains(got, "PLAIN") || strings.Contains(got, "SECRET") {
		t.Fatalf("logged:\n%s", got)
	}
	// As the chips said: one taken off (×), one hidden asked for (+).
	os.WriteFile(plain, []byte("PLAIN2"), 0o600)
	os.WriteFile(secret, []byte("SECRET2"), 0o600)
	ws.send(map[string]any{"t": "send", "text": "again " + plain + " and " + secret, "seq": 2, "key": "page:2",
		"skip": []string{plain}, "chosen": []string{secret}})
	ws.next("sent", typ("sent"))
	waitFor(t, "idle", wt.h.idle)
	got = strings.Join(logged(wt.c), "\n")
	if strings.Contains(got, "PLAIN2") || !strings.Contains(got, "SECRET2") {
		t.Fatalf("logged:\n%s", got)
	}
}

// An edit to a queued message that has already gone to Opt comes back to
// the input, with why, instead of being lost.
func TestWebEditGone(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "edit", "id": 12345, "text": "my edit"})
	u := ws.next("restore", typ("restore"))
	if u["text"] != "my edit" || !strings.Contains(u["note"].(string), "already gone") {
		t.Fatalf("restore: %v", u)
	}
}

// The model picker: Claude Code's models, and a choice the bar shows.
func TestWebModel(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "models"})
	if u := ws.next("models", typ("models")); len(u["models"].([]any)) != 3 {
		t.Fatalf("models: %v", u)
	}
	ws.send(map[string]any{"t": "model", "model": "default", "effort": "low", "save": true})
	if u := ws.next("chosen", typ("notice")); u["s"] != "Turns: default, effort low, from now on." {
		t.Fatalf("a choice: %v", u)
	}
	wt.c.hub.mu.Lock() // the bar's update went out before the notice
	st := wt.c.hub.status
	wt.c.hub.mu.Unlock()
	if st.Model != "default" || st.Effort != "low" {
		t.Fatalf("the bar: %+v", st)
	}
	if s := loadSettings(wt.c.dir); s.Model != "default" {
		t.Fatalf("saved %+v", s)
	}
	ws.send(map[string]any{"t": "model", "model": "default", "effort": "lots"})
	if u := ws.next("refused", typ("notice")); u["level"] != "warn" {
		t.Fatalf("a bad effort: %v", u)
	}
}

// The page asks for the plan's limits; they come in the bar's status.
func TestWebUsage(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "usage"})
	u := ws.next("limits", func(u map[string]any) bool {
		s, _ := u["status"].(map[string]any)
		return u["t"] == "status" && s["limits"] != nil
	})
	if ls := u["status"].(map[string]any)["limits"].([]any); len(ls) < 2 || ls[0].(map[string]any)["name"] != "five_hour" || ls[0].(map[string]any)["used"] != 12.0 {
		t.Fatalf("limits: %v", ls)
	}
}

// The agents rail opens a subagent: its log comes as steps, then each step
// as it is written.
func TestWebSteps(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	a := wt.h.agent
	if err := os.MkdirAll(filepath.Join(wt.c.dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	a.agentWrite("Scout", "task", "look around")
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "steps", "name": "[Scout]"})
	u := ws.next("steps", typ("steps"))
	if s := u["steps"].([]any); u["name"] != "Scout" || len(s) != 1 || s[0].(map[string]any)["text"] != "look around" {
		t.Fatalf("steps: %v", u)
	}
	a.agentWrite("Scout", "tool", `Bash {"command":"ls"}`)
	u = ws.next("a step", typ("step"))
	if s := u["step"].(map[string]any); u["name"] != "Scout" || s["kind"] != "tool" {
		t.Fatalf("step: %v", u)
	}
	ws.send(map[string]any{"t": "steps", "name": "Nobody"})
	ws.next("no log", func(u map[string]any) bool {
		return u["t"] == "notice" && strings.HasPrefix(u["s"].(string), "No log for [Nobody]")
	})
}
