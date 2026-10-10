package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Claude Code's own lists, as its terminal shows them in /model, /mcp and
// /skills, come from a call that takes control requests only and never
// calls a model: started when a page first asks, and stopped after a while
// unused. It runs with a turn's setup, so it sees the same MCP servers and
// skills, and it stays up long enough to finish an MCP server's sign-in in
// the browser.
type control struct {
	p       *proc
	mu      sync.Mutex
	seq     int
	waiting map[string]chan controlReply
	idle    *time.Timer
}

type controlReply struct {
	v   jsontext.Value
	err error
}

// controlResponse is Claude Code's answer to a control request.
type controlResponse struct {
	Subtype   string         `json:"subtype"` // success or error
	RequestID string         `json:"request_id"`
	Response  jsontext.Value `json:"response"`
	Error     string         `json:"error"`
}

const controlIdle = 10 * time.Minute

// control sends Claude Code one control request, such as
// {"subtype": "mcp_status"}, and returns its response.
func (a *Agent) control(ctx context.Context, req any) (jsontext.Value, error) {
	cl, err := a.controller()
	if err != nil {
		return nil, err
	}
	return cl.do(ctx, req)
}

func (a *Agent) controller() (*control, error) {
	a.ctlMu.Lock()
	defer a.ctlMu.Unlock()
	if cl := a.ctl; cl != nil {
		cl.idle.Reset(controlIdle)
		return cl, nil
	}
	a.mu.Lock()
	model := a.model
	a.mu.Unlock()
	p, err := a.start(a.life, model, "", roleControl)
	if err != nil {
		return nil, err
	}
	cl := &control{p: p, waiting: map[string]chan controlReply{}}
	cl.idle = time.AfterFunc(controlIdle, p.close) // with its input closed, Claude Code exits
	a.ctl = cl
	go func() {
		cl.read()
		a.ctlMu.Lock()
		if a.ctl == cl {
			a.ctl = nil
		}
		a.ctlMu.Unlock()
	}()
	return cl, nil
}

// read hands each response to its request until the call ends, then fails
// the requests left.
func (cl *control) read() {
	for {
		e, ok := cl.p.next()
		if !ok {
			break
		}
		if e.Type != "control_response" {
			continue
		}
		r := e.Response
		cl.mu.Lock()
		ch := cl.waiting[r.RequestID]
		delete(cl.waiting, r.RequestID)
		cl.mu.Unlock()
		if ch == nil {
			continue
		}
		if r.Subtype == "error" {
			ch <- controlReply{err: errors.New(r.Error)}
		} else {
			ch <- controlReply{v: r.Response}
		}
	}
	cl.idle.Stop()
	err := fmt.Errorf("Claude Code exited: %v", cl.p.wait())
	cl.mu.Lock()
	for id, ch := range cl.waiting {
		ch <- controlReply{err: err}
		delete(cl.waiting, id)
	}
	cl.p = nil
	cl.mu.Unlock()
}

func (cl *control) do(ctx context.Context, req any) (jsontext.Value, error) {
	ch := make(chan controlReply, 1)
	cl.mu.Lock()
	if cl.p == nil {
		cl.mu.Unlock()
		return nil, errors.New("Claude Code exited")
	}
	cl.seq++
	id := fmt.Sprint("opt-", cl.seq)
	cl.waiting[id] = ch
	p := cl.p
	cl.mu.Unlock()
	drop := func() {
		cl.mu.Lock()
		delete(cl.waiting, id)
		cl.mu.Unlock()
	}
	if !p.control(id, req) {
		drop()
		return nil, errors.New("Claude Code is not taking requests")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// control writes a control request to the call; false once its input is closed.
func (p *proc) control(id string, req any) bool {
	b, err := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": req})
	if err != nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stdin == nil {
		return false
	}
	_, err = p.stdin.Write(append(b, '\n'))
	return err == nil
}

// A model as Claude Code's /model lists it.
type modelInfo struct {
	Value          string   `json:"value"` // what --model takes
	DisplayName    string   `json:"displayName"`
	Description    string   `json:"description"`
	SupportsEffort bool     `json:"supportsEffort"`
	Efforts        []string `json:"supportedEffortLevels"`
}

// models is Claude Code's list of models, asked once a run.
func (a *Agent) models(ctx context.Context) ([]modelInfo, error) {
	a.mu.Lock()
	ms := a.modelList
	a.mu.Unlock()
	if ms != nil {
		return ms, nil
	}
	v, err := a.control(ctx, map[string]any{"subtype": "list_models"})
	if err != nil {
		return nil, err
	}
	var r struct {
		Models []modelInfo `json:"models"`
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return nil, err
	}
	if r.Models == nil {
		r.Models = []modelInfo{}
	}
	a.mu.Lock()
	a.modelList = r.Models
	a.mu.Unlock()
	return r.Models, nil
}

// settings are what the page's pickers keep for later runs, in
// <dir>/settings.json. A flag given on the command line wins over them.
type settings struct {
	Model  string `json:"model,omitzero"`
	Effort string `json:"effort,omitzero"`
}

func loadSettings(dir string) settings {
	var s settings
	if b, err := os.ReadFile(filepath.Join(dir, "settings.json")); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func saveSettings(dir string, s settings) error {
	b, err := json.Marshal(s, jsontext.Multiline(true))
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "settings.json"), append(b, '\n'))
}

// choose sets the turns' model and effort; save keeps them for later runs.
func (a *Agent) choose(model, effort string, save bool) error {
	if !modelName.MatchString(model) {
		return fmt.Errorf("%q is no model name", model)
	}
	if !validEffort(effort) {
		return fmt.Errorf("effort %q is none of %s", effort, joinEfforts())
	}
	a.mu.Lock()
	a.model, a.effort = model, effort
	a.mu.Unlock()
	if save {
		return saveSettings(a.chat.dir, settings{Model: model, Effort: effort})
	}
	return nil
}

// writeAtomic replaces path with b whole: a crash leaves the old file or the new.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
