package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Voice messages: the page records one (Alt+K), posts it to /voice, and
// gets back the attachment to send, its words transcribed by Whisper on
// Groq. The words are kept beside the audio, as files/<hash>.<ext>.txt, so
// the message's mark holds them wherever the message is composed: when
// sent, or sent again after a stop handed it back. They are on only when
// GROQ_API_KEY is set: the audio goes to Groq.

const voiceMax = 25 << 20 // bytes: the most Groq takes

var audioExt = regexp.MustCompile(`\.(webm|ogg|oga|opus|m4a|mp4|mp3|wav|flac)$`)

type transcriber struct {
	key, model, url string
	client          *http.Client
}

// newTranscriber is Groq's Whisper, with the key in GROQ_API_KEY; nil when
// it is unset.
func newTranscriber() *transcriber {
	key := strings.TrimSpace(os.Getenv("GROQ_API_KEY"))
	if key == "" {
		return nil
	}
	return &transcriber{key: key, model: "whisper-large-v3", url: "https://api.groq.com/openai/v1/audio/transcriptions",
		client: &http.Client{Timeout: 2 * time.Minute}}
}

// transcribe returns the words spoken in the audio file at path.
func (t *transcriber) transcribe(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("model", t.model)
	mw.WriteField("response_format", "json")
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return "", err
	}
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, "POST", t.url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := t.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Groq: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Text  string `json:"text"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(b, &out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b[:min(len(b), 200)]))
		}
		return "", fmt.Errorf("Groq answered %s: %s", resp.Status, msg)
	}
	return strings.TrimSpace(out.Text), nil
}

// transcript is the words kept beside an audio attachment at path, if any.
func transcript(path string) (string, bool) {
	if !audioExt.MatchString(path) {
		return "", false
	}
	b, err := os.ReadFile(path + ".txt")
	if err != nil {
		return "", false
	}
	// The words go inside the mark: they can't close it.
	return strings.ReplaceAll(strings.TrimSpace(string(b)), "</attachment>", "</attachment >"), true
}

// voice answers whether voice messages are on (GET), or saves a recording
// a page sends, its name in X-Name, transcribes it and answers with the
// attachment to send (POST). Whatever goes wrong after the audio is saved,
// the attachment comes back with the error, so the recording is not lost.
func (w *webServer) voice(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Content-Type", "application/json")
	if r.Method == "GET" {
		if w.speech == nil {
			json.MarshalWrite(rw, map[string]any{"on": false, "why": "Voice messages are off: set GROQ_API_KEY (from console.groq.com) and restart opt-chat. The audio goes to Groq to be transcribed."})
			return
		}
		json.MarshalWrite(rw, map[string]any{"on": true})
		return
	}
	name, err := url.PathUnescape(r.Header.Get("X-Name"))
	if err != nil || !audioExt.MatchString(name) {
		http.Error(rw, "not a recording: name it .webm, .ogg or .m4a", http.StatusBadRequest)
		return
	}
	if w.speech == nil {
		http.Error(rw, "voice messages are off: set GROQ_API_KEY and restart opt-chat", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(rw, r.Body, voiceMax)
	at, err := w.c.save(r.Body, name)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	reply := struct {
		attachment
		Text  string `json:"text,omitzero"`
		Error string `json:"error,omitzero"`
	}{attachment: at}
	path := filepath.Join(w.c.dir, at.Name)
	if words, ok := transcript(path); ok { // sent before: transcribed already
		reply.Text = words
	} else if words, err := w.speech.transcribe(r.Context(), path); err != nil {
		reply.Error = err.Error()
	} else if words == "" {
		reply.Error = "no words heard in it"
	} else if err := os.WriteFile(path+".txt", []byte(words+"\n"), 0o600); err != nil {
		reply.Error = err.Error()
	} else {
		reply.Text = words
	}
	json.MarshalWrite(rw, reply)
}
