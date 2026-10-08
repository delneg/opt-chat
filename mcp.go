package main

import (
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
)

// serveMCP serves zoom and date to Claude Code over MCP (streamable HTTP,
// plain JSON replies) on a local port, behind a random secret in the path.
// It returns the --mcp-config that points at it.
func serveMCP(c *Chat) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	path := "/" + rand.Text() + "/mcp"
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     jsontext.Value `json:"id"`
			Method string         `json:"method"`
			Params jsontext.Value `json:"params"`
		}
		if err := json.UnmarshalRead(r.Body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(req.ID) == 0 { // a notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if res, ok := mcpResult(c, req.Method, req.Params); ok {
			resp["result"] = res
		} else {
			resp["error"] = map[string]any{"code": -32601, "message": "Method not found: " + req.Method}
		}
		w.Header().Set("Content-Type", "application/json")
		json.MarshalWrite(w, resp)
	})
	go http.Serve(ln, mux)
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"opt": map[string]any{"type": "http", "url": fmt.Sprintf("http://%s%s", ln.Addr(), path)},
	}})
	return string(cfg), nil
}

var mcpTools = []map[string]any{
	{"name": "zoom", "description": "Open the line id+n of the view into the two lines of n/2 under it; n = 1 gives the message whole.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"id", "n"}, "properties": map[string]any{
			"id": map[string]any{"type": "integer"}, "n": map[string]any{"type": "integer"}}}},
	{"name": "date", "description": "The date and time of message id.",
		"inputSchema": map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{
			"id": map[string]any{"type": "integer"}}}},
}

func mcpResult(c *Chat, method string, params jsontext.Value) (any, bool) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		return map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "opt", "version": "1"}}, true
	case "ping":
		return map[string]any{}, true
	case "tools/list":
		return map[string]any{"tools": mcpTools}, true
	case "tools/call":
		var p struct {
			Name string `json:"name"`
			Args struct {
				ID int `json:"id"`
				N  int `json:"n"`
			} `json:"arguments"`
		}
		json.Unmarshal(params, &p)
		var out string
		switch p.Name {
		case "zoom":
			out = c.Zoom(p.Args.ID, p.Args.N)
		case "date":
			out = c.Date(p.Args.ID)
		default:
			out = "No tool " + p.Name + "."
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": out}}}, true
	}
	return nil, false
}
