package main

import (
	"encoding/json/v2"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCP(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("user", "hi")
	png, err := c.saveImage([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	if err != nil {
		t.Fatal(err)
	}
	c.Log("user", "see", png)
	pdf, _, err := c.saveFile(strings.NewReader("%PDF-1.4\n"), "pdf")
	if err != nil {
		t.Fatal(err)
	}
	c.Log("user", "read", pdf)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	cfg, agentCfg, err := serveMCP(a)
	if err != nil {
		t.Fatal(err)
	}
	urlOf := func(cfg string) string {
		var conf struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		if err := json.Unmarshal([]byte(cfg), &conf); err != nil {
			t.Fatal(err)
		}
		return conf.MCPServers["opt"].URL
	}
	url, agentURL := urlOf(cfg), urlOf(agentCfg)
	post := func(url, body string) (int, map[string]any) {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.UnmarshalRead(resp.Body, &out)
		return resp.StatusCode, out
	}
	call := func(body string) (int, map[string]any) { return post(url, body) }
	content := func(out map[string]any) []any { return out["result"].(map[string]any)["content"].([]any) }
	text := func(out map[string]any) string { return content(out)[0].(map[string]any)["text"].(string) }

	if code, out := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); code != 200 ||
		out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	if code, _ := call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	if _, out := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); len(out["result"].(map[string]any)["tools"].([]any)) != 5 {
		t.Fatalf("tools/list: %v", out)
	}
	for args, want := range map[string]string{
		`{"name":"zoom","arguments":{"id":0,"n":1}}`:                           "0+1|user: hi",
		`{"name":"zoom","arguments":{"id":"0","n":1}}`:                         "0+1|user: hi",
		`{"name":"zoom","arguments":{"id":"Nobody"}}`:                          "No subagent [Nobody].",
		`{"name":"tell","arguments":{"name":"X","message":"m"}}`:               "No subagent [X] is taking messages. One ends 30m0s after its report; a new one can read its chat with zoom(\"X\").",
		`{"name":"spawn","arguments":{"name":"X","task":" "}}`:                 "No task given.",
		`{"name":"spawn","arguments":{"name":"X","task":"t","effort":"lots"}}`: "effort must be one of low, medium, high, xhigh, max, or left out.",
	} {
		if _, out := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` + args + `}`); text(out) != want {
			t.Fatalf("%s: %q, want %q", args, text(out), want)
		}
	}
	if _, out := call(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"search","arguments":{"text":"SEE"}}}`); !strings.HasPrefix(text(out), "1 message holds \"SEE\"") {
		t.Fatalf("search: %v", out)
	}
	if _, out := call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"date","arguments":{"id":0}}}`); !strings.Contains(text(out), "20") {
		t.Fatalf("date: %v", out)
	}
	_, out := call(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"zoom","arguments":{"id":1,"n":1}}}`)
	if img := content(out); len(img) != 2 || text(out) != "1+1|user: see" || img[1].(map[string]any)["type"] != "image" ||
		img[1].(map[string]any)["mimeType"] != "image/png" {
		t.Fatalf("zoom of a message with an image: %v", out)
	}
	_, out = call(`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"zoom","arguments":{"id":2,"n":1}}}`)
	if c := content(out); len(c) != 2 || c[1].(map[string]any)["type"] != "text" || c[1].(map[string]any)["text"] != "[PDF: "+filepath.Join(a.chat.dir, pdf)+"]" {
		t.Fatalf("zoom of a message with a PDF: %v", out)
	}
	for _, args := range []string{`{"name":"spawn","arguments":{"name":"X","task":"t"}}`, `{"name":"tell","arguments":{"name":"X","message":"m"}}`} {
		if _, out := post(agentURL, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":`+args+`}`); text(out) != "Only Opt starts and tells subagents." {
			t.Fatalf("a subagent's %s: %v", args, out)
		}
	}
	if _, out := post(agentURL, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"zoom","arguments":{"id":0,"n":1}}}`); text(out) != "0+1|user: hi" {
		t.Fatalf("a subagent's zoom: %v", out)
	}
	if _, out := call(`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`); out["error"] == nil {
		t.Fatalf("unknown method: %v", out)
	}
	if resp, _ := http.Post(strings.Replace(url, "/mcp", "/nope", 1), "application/json", strings.NewReader("{}")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong secret path: %d", resp.StatusCode)
	}
}
