package main

import (
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"strconv"
)

// serveMCP serves zoom, date, spawn and tell to Claude Code over MCP (streamable HTTP,
// plain JSON replies) on a local port, behind a random secret in the path.
// It returns the --mcp-config that points at it.
func serveMCP(a *Agent) (string, error) {
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
		if res, ok := mcpResult(a, req.Method, req.Params); ok {
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

func schema(required []string, props map[string]any) map[string]any {
	return map[string]any{"type": "object", "required": required, "properties": props}
}

var (
	str  = map[string]any{"type": "string"}
	num  = map[string]any{"type": "integer"}
	tool = func(name, desc string, input map[string]any) map[string]any {
		return map[string]any{"name": name, "description": desc, "inputSchema": input}
	}
	mcpTools = []map[string]any{
		tool("zoom", "Open the line id+n of the view into the two lines of n/2 under it; n = 1 gives the message whole. "+
			`zoom("Name") gives an agent's whole chat.`,
			schema([]string{"id"}, map[string]any{"id": map[string]any{"type": []string{"integer", "string"}}, "n": num})),
		tool("date", "The date and time of message id.", schema([]string{"id"}, map[string]any{"id": num})),
		tool("spawn", `Start an agent on a task, in the background, and return at once. Its report comes back as a message starting "[Name]". Never wait for it.`,
			schema([]string{"name", "task"}, map[string]any{"name": str, "task": str})),
		tool("tell", "Send a running agent a message; it reaches the agent between its tool calls.",
			schema([]string{"name", "message"}, map[string]any{"name": str, "message": str})),
	}
)

func mcpResult(a *Agent, method string, params jsontext.Value) (any, bool) {
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
				ID      jsontext.Value `json:"id"`
				N       int            `json:"n"`
				Name    string         `json:"name"`
				Task    string         `json:"task"`
				Message string         `json:"message"`
			} `json:"arguments"`
		}
		json.Unmarshal(params, &p)
		return map[string]any{"content": []map[string]any{{"type": "text", "text": callTool(a, p.Name, p.Args.ID, p.Args.N, p.Args.Name, p.Args.Task, p.Args.Message)}}}, true
	}
	return nil, false
}

func callTool(a *Agent, tool string, id jsontext.Value, n int, name, task, msg string) string {
	switch tool {
	case "zoom":
		var agent string
		if json.Unmarshal(id, &agent) == nil {
			if i, err := strconv.Atoi(agent); err == nil { // a message id sent as a string
				return a.chat.Zoom(i, n)
			}
			return a.chat.ZoomAgent(agent)
		}
		var i int
		if json.Unmarshal(id, &i) != nil {
			return "id must be a message id or an agent's name."
		}
		return a.chat.Zoom(i, n)
	case "date":
		var i int
		if json.Unmarshal(id, &i) != nil {
			return "id must be a message id."
		}
		return a.chat.Date(i)
	case "spawn":
		return a.Spawn(name, task)
	case "tell":
		return a.Tell(name, msg)
	}
	return "No tool " + tool + "."
}
