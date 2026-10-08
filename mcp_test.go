package main

import (
	"encoding/json/v2"
	"net/http"
	"strings"
	"testing"
)

func TestMCP(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("user", "hi")
	a := &Agent{chat: c, running: map[string]*proc{}}
	cfg, err := serveMCP(a)
	if err != nil {
		t.Fatal(err)
	}
	var conf struct {
		MCPServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(cfg), &conf); err != nil {
		t.Fatal(err)
	}
	url := conf.MCPServers["opt"].URL
	call := func(body string) (int, map[string]any) {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.UnmarshalRead(resp.Body, &out)
		return resp.StatusCode, out
	}
	text := func(out map[string]any) string {
		return out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	}

	if code, out := call(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); code != 200 ||
		out["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	if code, _ := call(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted {
		t.Fatalf("notification: %d", code)
	}
	if _, out := call(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); len(out["result"].(map[string]any)["tools"].([]any)) != 4 {
		t.Fatalf("tools/list: %v", out)
	}
	for args, want := range map[string]string{
		`{"name":"zoom","arguments":{"id":0,"n":1}}`:             "0+1|user: hi",
		`{"name":"zoom","arguments":{"id":"0","n":1}}`:           "0+1|user: hi",
		`{"name":"zoom","arguments":{"id":"Nobody"}}`:            "No agent [Nobody].",
		`{"name":"tell","arguments":{"name":"X","message":"m"}}`: "No agent [X] is taking messages.",
		`{"name":"spawn","arguments":{"name":"X","task":" "}}`:   "No task given.",
	} {
		if _, out := call(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":` + args + `}`); text(out) != want {
			t.Fatalf("%s: %q, want %q", args, text(out), want)
		}
	}
	if _, out := call(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"date","arguments":{"id":0}}}`); !strings.Contains(text(out), "20") {
		t.Fatalf("date: %v", out)
	}
	if _, out := call(`{"jsonrpc":"2.0","id":5,"method":"resources/list"}`); out["error"] == nil {
		t.Fatalf("unknown method: %v", out)
	}
	if resp, _ := http.Post(strings.Replace(url, "/mcp", "/nope", 1), "application/json", strings.NewReader("{}")); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong secret path: %d", resp.StatusCode)
	}
}
