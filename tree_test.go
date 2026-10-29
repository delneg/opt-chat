package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The page's view mode gets the view line by line, a line not summarized
// yet marked, and opens a line into its halves, down to the message whole.
func TestMemViewZoom(t *testing.T) {
	t.Parallel()
	c, err := openChat(t.TempDir()) // no compactions: a long message stays unsummarized
	if err != nil {
		t.Fatal(err)
	}
	c.Log("user", "hi")
	c.Log("opt", "hello")
	c.Log("echo", strings.Repeat("x", 600))

	v := c.memView()
	if len(v) == 0 || v[0].ID != 0 {
		t.Fatalf("view %v", v)
	}
	last := v[len(v)-1]
	if last.ID != 2 || last.N != 1 || last.Built || !strings.HasPrefix(last.Text, "echo: xxx") || strings.Contains(last.Text, unbuilt) {
		t.Fatalf("the unsummarized message's line: %+v", last)
	}
	if end := v[len(v)-1].ID + v[len(v)-1].N; end != 3 {
		t.Fatalf("the view covers %d messages, want 3", end)
	}
	bytes := 0
	for _, l := range v {
		bytes += l.Bytes
	}
	c.mu.Lock()
	want := len(strings.Join(c.callView(), "\n")) + 1
	c.mu.Unlock()
	if bytes != want {
		t.Fatalf("the lines' bytes add up to %d, a call's view is %d", bytes, want)
	}
	z := c.memZoom(0, 2)
	if z.Error != "" || len(z.Lines) != 2 || z.Lines[0].Text != "user: hi" || !z.Lines[0].Built || z.Lines[1].ID != 1 || z.Lines[1].N != 1 {
		t.Fatalf("zoom(0, 2) = %+v", z)
	}
	if z := c.memZoom(2, 1); z.Msg == nil || z.Msg.Kind != "echo" || len(z.Msg.Text) != 600 {
		t.Fatalf("zoom(2, 1) = %+v", z)
	}
	for _, bad := range [][2]int{{1, 2}, {0, 3}, {2, 2}, {-1, 1}, {3, 1}} {
		if z := c.memZoom(bad[0], bad[1]); z.Error == "" || z.Lines != nil || z.Msg != nil {
			t.Fatalf("zoom%v = %+v", bad, z)
		}
	}
}

// /tree shows the view, the messages and each level, a page at a time,
// only with the key; the page's socket asks for the view and its zooms.
func TestWebTree(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	for i := range 5 {
		wt.c.Log("user", "line <b>"+strings.Repeat("y", i)+"</b>")
	}
	ck := map[string]string{"Cookie": wt.cookie}
	get := func(path string, hdr map[string]string) (int, string) {
		resp := wt.do("GET", path, nil, hdr)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/tree", nil); code != http.StatusUnauthorized {
		t.Fatalf("/tree without the key: %d", code)
	}
	for path, want := range map[string]string{
		"/tree":        "4&#43;1 · ", // html/template escapes +
		"/tree?s=msgs": "#4 · ",
		"/tree?s=0":    "line &lt;b&gt;yyyy&lt;/b&gt;", // escaped
		"/tree?s=2":    "Level 2: each line summarizes 4 messages",
	} {
		if code, body := get(path, ck); code != http.StatusOK || !strings.Contains(body, want) || strings.Contains(body, "<b>y") {
			t.Fatalf("%s: %d %.300s", path, code, body)
		}
	}
	for _, path := range []string{"/tree?s=3", "/tree?s=msgs&p=1", "/tree?s=x", "/tree?p=-1", "/tree?p=1"} {
		if code, _ := get(path, ck); code != http.StatusNotFound {
			t.Fatalf("%s: %d", path, code)
		}
	}

	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.send(map[string]any{"t": "memview"})
	if u := ws.next("memview", typ("memview")); len(u["lines"].([]any)) == 0 {
		t.Fatalf("memview: %v", u)
	}
	ws.send(map[string]any{"t": "zoom", "id": 0, "n": 2})
	if u := ws.next("zoom", typ("zoom")); len(u["lines"].([]any)) != 2 || u["n"].(float64) != 2 {
		t.Fatalf("zoom: %v", u)
	}
	ws.send(map[string]any{"t": "zoom", "id": 4, "n": 1})
	if u := ws.next("zoom", typ("zoom")); u["msg"].(map[string]any)["text"] != "line <b>yyyy</b>" {
		t.Fatalf("zoom to a message: %v", u)
	}
}
