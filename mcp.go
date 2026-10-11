package main

import (
	"crypto/rand"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
)

// serveMCP serves zoom, date, search, spawn and tell to Claude Code over MCP (streamable HTTP,
// plain JSON replies) on a local port, behind a random secret in the path.
// It returns the --mcp-config that points at it, and one for subagents.
func serveMCP(a *Agent) (string, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", err
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
		if res, ok := mcpResult(a, req.Method, req.Params, r.URL.Query().Has("agent")); ok {
			resp["result"] = res
		} else {
			resp["error"] = map[string]any{"code": -32601, "message": "Method not found: " + req.Method}
		}
		w.Header().Set("Content-Type", "application/json")
		json.MarshalWrite(w, resp)
	})
	go http.Serve(ln, mux)
	cfg := func(url string) string {
		b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"opt": map[string]any{"type": "http", "url": url}}})
		return string(b)
	}
	url := fmt.Sprintf("http://%s%s", ln.Addr(), path)
	return cfg(url), cfg(url + "?agent"), nil
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
		tool("zoom", "Open the line id+n of the view into the two lines of n/2 under it; n = 1 gives the message whole, with its images. "+
			`zoom("Name") gives a subagent's whole chat.`,
			schema([]string{"id"}, map[string]any{"id": map[string]any{"type": []string{"integer", "string"}}, "n": num})),
		tool("date", "The date and time of message id.", schema([]string{"id"}, map[string]any{"id": num})),
		tool("search", "Find the messages of the whole chat that hold text (a word, a name, a number, an error; any case), newest first, 20 at a time: "+
			"each with a snippet, its date, and the view's line that holds it, to zoom into. before: go on from this message id.",
			schema([]string{"text"}, map[string]any{"text": str, "before": num})),
		tool("spawn", `Start a subagent on a task, in the background, and return at once. Its report comes back as a message starting "[Name]". Never wait for it. `+
			"effort: how long it thinks first; more is smarter and slower. Leave it out for the default. "+
			"model: the Claude it runs on: opus (the default), fable, sonnet, haiku, or a full model name; leave it out unless the user asks for one. "+
			"autocompact: for a long task, a context size in tokens: once the subagent has reported past it, opt-chat compacts it at rest, never mid-step, and its next message (a tell) brings the view and its task again; leave it out for the user's setting (/autocompact).",
			schema([]string{"name", "task"}, map[string]any{"name": str, "task": str, "effort": map[string]any{"type": "string", "enum": efforts}, "model": str,
				"autocompact": map[string]any{"type": "integer", "minimum": windowMin, "maximum": windowMax}})),
		tool("tell", "Send a subagent a message. A running one gets it between its tool calls; one that reported in the last 30 minutes goes on with it, "+
			"with all it knew, and reports again.",
			schema([]string{"name", "message"}, map[string]any{"name": str, "message": str})),
	}
)

func mcpResult(a *Agent, method string, params jsontext.Value, fromAgent bool) (any, bool) {
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
			Name string   `json:"name"`
			Args toolArgs `json:"arguments"`
		}
		json.Unmarshal(params, &p)
		text, images := callTool(a, fromAgent, p.Name, p.Args)
		content := []map[string]any{}
		for _, b := range a.chat.interleave(text, images) { // each image where its message has it
			switch {
			case b.Source == nil: // text, or a file lost
				content = append(content, map[string]any{"type": "text", "text": b.Text})
			case b.Type == "document": // a tool result carries no PDF: its mark names its path, which Read opens
				content = append(content, map[string]any{"type": "text", "text": "[PDF: " + filepath.Join(a.chat.dir, b.file) + "]"})
			default:
				content = append(content, map[string]any{"type": "image", "data": b.Source.Data, "mimeType": b.Source.MediaType})
			}
		}
		return map[string]any{"content": content}, true
	}
	return nil, false
}

type toolArgs struct {
	ID      jsontext.Value `json:"id"`
	N       int            `json:"n"`
	Name    string         `json:"name"`
	Task    string         `json:"task"`
	Message string         `json:"message"`
	Effort  string         `json:"effort"`
	Model   string         `json:"model"`
	Window  int            `json:"autocompact"`
	Text    string         `json:"text"`
	Before  int            `json:"before"`
}

// callTool runs one tool and returns its text and the images that go with it.
func callTool(a *Agent, fromAgent bool, tool string, x toolArgs) (string, []string) {
	id, n := x.ID, x.N
	var s string
	isName := json.Unmarshal(id, &s) == nil
	i, err := strconv.Atoi(s) // a message id sent as a string
	if !isName {
		err = json.Unmarshal(id, &i)
	} else if err == nil {
		isName = false
	}
	switch {
	case tool == "zoom" && isName:
		return a.chat.ZoomAgent(s), nil
	case tool == "zoom" && err != nil:
		return "id must be a message id or a subagent's name.", nil
	case tool == "zoom" && n == 0: // n left out: the message
		return a.chat.Zoom(i, 1)
	case tool == "zoom":
		return a.chat.Zoom(i, n)
	case tool == "date" && (isName || err != nil):
		return "id must be a message id.", nil
	case tool == "date":
		return a.chat.Date(i), nil
	case tool == "search":
		return a.chat.Search(x.Text, x.Before), nil
	case (tool == "spawn" || tool == "tell") && fromAgent:
		return "Only Opt starts and tells subagents.", nil
	case tool == "spawn":
		return a.Spawn(x.Name, x.Task, x.Effort, x.Model, x.Window), nil
	case tool == "tell":
		return a.Tell(x.Name, x.Message), nil
	}
	return "No tool " + tool + ".", nil
}
