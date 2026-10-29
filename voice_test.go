package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A recording posted to /voice is saved, transcribed once, and its words go
// in its message's mark; it plays from /files/.
func TestVoice(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ck := map[string]string{"Cookie": wt.cookie}
	var off struct {
		On  bool   `json:"on"`
		Why string `json:"why"`
	}
	json.UnmarshalRead(wt.do("GET", "/voice", nil, ck).Body, &off)
	if off.On || !strings.Contains(off.Why, "GROQ_API_KEY") {
		t.Fatalf("with no key: %+v", off)
	}

	calls, fail := 0, false
	groq := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		f, _, err := r.FormFile("file")
		if r.Header.Get("Authorization") != "Bearer gsk_test" || r.FormValue("model") != "whisper-large-v3" || err != nil {
			t.Errorf("request: %v %v", r.Header, err)
		}
		if b, _ := io.ReadAll(f); !strings.HasPrefix(string(b), "OggS\x00fake audio") {
			t.Errorf("audio %q", b)
		}
		if fail {
			rw.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(rw, `{"error":{"message":"rate limited"}}`)
			return
		}
		io.WriteString(rw, `{"text":" Hello there, Opt. "}`)
	}))
	defer groq.Close()
	wt.w.speech = &transcriber{key: "gsk_test", model: "whisper-large-v3", url: groq.URL, client: groq.Client()}

	type reply struct {
		attachment
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	post := func(body string) reply {
		t.Helper()
		resp := wt.do("POST", "/voice", strings.NewReader(body), map[string]string{"Cookie": wt.cookie, "X-Name": "voice%200%3A03.ogg"})
		var r reply
		if err := json.UnmarshalRead(resp.Body, &r); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%d: %v", resp.StatusCode, err)
		}
		return r
	}
	r := post("OggS\x00fake audio")
	if r.Text != "Hello there, Opt." || r.Error != "" || r.Orig != "voice 0:03.ogg" || !safeName.MatchString(r.Name) || calls != 1 {
		t.Fatalf("reply %+v, %d calls", r, calls)
	}
	if again := post("OggS\x00fake audio"); again.Text != r.Text || calls != 1 { // sent again: its words were kept
		t.Fatalf("again %+v, %d calls", again, calls)
	}

	ups, err := wt.c.uploaded([]attachment{r.attachment})
	if err != nil {
		t.Fatal(err)
	}
	full := wt.c.compose("", ups).full()
	if !strings.HasPrefix(full, `<attachment name="voice 0:03.ogg" path="`+filepath.Join(wt.c.dir, r.Name)+`"`) || !strings.HasSuffix(full, ">\nHello there, Opt.\n</attachment>") {
		t.Fatalf("mark %q", full)
	}
	if s := stripAttachments(full); s != "" {
		t.Fatalf("stripped %q", s)
	}

	resp := wt.do("GET", "/"+r.Name, nil, ck)
	if b, _ := io.ReadAll(resp.Body); resp.Header.Get("Content-Type") != "audio/ogg" || string(b) != "OggS\x00fake audio" {
		t.Fatalf("served as %q: %q", resp.Header.Get("Content-Type"), b)
	}
	if resp := wt.do("GET", "/"+r.Name+".txt", nil, ck); resp.StatusCode != 404 { // the words go in the mark alone
		t.Fatalf("words served: %d", resp.StatusCode)
	}

	fail = true // the audio stays, with what went wrong
	r = post("OggS\x00fake audio, longer")
	if !strings.Contains(r.Error, "rate limited") || r.Text != "" || calls != 2 {
		t.Fatalf("failed: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(wt.c.dir, r.Name)); err != nil {
		t.Fatal(err)
	}
	if full := wt.c.compose("", []attachment{r.attachment}).full(); !strings.HasSuffix(full, "/>") {
		t.Fatalf("no words, yet %q", full)
	}

	if resp := wt.do("POST", "/voice", bytes.NewReader([]byte("x")), map[string]string{"Cookie": wt.cookie, "X-Name": "notes.txt"}); resp.StatusCode != 400 {
		t.Fatalf("not audio: %d", resp.StatusCode)
	}
}
