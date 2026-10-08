package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Agents run in the background, so a long task never blocks the chat. An
// agent is a fresh call whose first message is the view, then its task. Its
// own steps stay in its own log, agents/<Name>.log, out of the chat; its
// final reply comes back to the chat as one work message, "[Name] report".

var badName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (a *Agent) agentLog(name string) string { return filepath.Join(a.chat.dir, "agents", name+".log") }

// Spawn starts an agent and returns at once.
func (a *Agent) Spawn(name, task string) string {
	if strings.TrimSpace(task) == "" {
		return "No task given."
	}
	name = strings.Trim(badName.ReplaceAllString(name, "-"), "-")
	if name == "" {
		name = "agent"
	}
	if !a.begin() {
		return "opt-chat is quitting."
	}
	a.mu.Lock()
	base := name
	for n := 2; ; n++ { // names are forever: zoom("Name") opens its log
		if _, taken := a.running[name]; !taken {
			if _, err := os.Stat(a.agentLog(name)); os.IsNotExist(err) {
				break
			}
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
	a.running[name] = nil // reserved until its call starts
	a.mu.Unlock()
	go func() {
		defer a.calls.Done() // after its report is in the chat
		a.runAgent(name, task)
	}()
	return fmt.Sprintf("Started [%s] in the background. Its report will come as a message starting \"[%s]\".", name, name)
}

// Tell hands a running agent a message, between its tool calls.
func (a *Agent) Tell(name, text string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.running[name]
	if p == nil {
		return fmt.Sprintf("No subagent [%s] is taking messages.", name)
	}
	a.agentWrite(name, "opt", text)
	if !p.send(textBlock(text)) {
		return fmt.Sprintf("[%s] has just finished.", name)
	}
	return "Sent."
}

func (a *Agent) agentWrite(name, kind, text string) {
	if err := appendRaw(a.agentLog(name), []byte(kind+": "+text+"\n\n")); err != nil {
		warn("[%s] log: %v", name, err)
	}
}

func (a *Agent) runAgent(name, task string) {
	report := func(text string) {
		if a.life.Err() != nil {
			text = "stopped: opt-chat quit before this subagent finished."
		}
		a.mu.Lock()
		delete(a.running, name)
		a.mu.Unlock()
		dim(fmt.Sprintf("[%s] done", name))
		if !strings.HasPrefix(text, "["+name+"]") { // as the prompt asks of a report
			text = "[" + name + "] " + text
		}
		a.report(text)
	}
	if err := os.MkdirAll(filepath.Join(a.chat.dir, "agents"), 0o700); err != nil {
		report("failed: " + err.Error())
		return
	}
	a.agentWrite(name, "task", task)
	dim(fmt.Sprintf("[%s] started", name))
	c := a.chat
	if c.settle(a.life, func(int) {}) != nil {
		report("")
		return
	}
	c.mu.Lock()
	lines := c.lines(c.view.keys)
	c.mu.Unlock()
	p, err := a.start(context.Background(), a.model, a.effort, roleAgent)
	if err != nil {
		report("failed to start: " + err.Error())
		return
	}
	acking := true
	each := func(e event) {
		if e.Parent != nil {
			return
		}
		for _, b := range e.blocks() {
			switch {
			case e.Type == "assistant" && b.Type == "text" && !acking:
				a.agentWrite(name, "agent", b.Text)
			case e.Type == "assistant" && b.Type == "tool_use":
				a.agentWrite(name, "tool", b.Name+" "+string(b.Input))
			case e.Type == "user" && b.Type == "tool_result":
				out, _ := resultContent(b.Content)
				a.agentWrite(name, "echo", clip(out, clipMax))
			}
		}
	}
	var u usage
	first, second := viewMessages(lines, []block{textBlock(fmt.Sprintf("You are subagent [%s]. Your task:\n%s", name, task))})
	if err := a.ack(p, first, &u, each); err != nil {
		p.wait()
		report("failed: " + err.Error())
		return
	}
	acking = false
	a.mu.Lock()
	p.send(second...)
	a.running[name] = p
	a.mu.Unlock()
	final := ""
	for {
		e, ok := p.next()
		if !ok {
			break
		}
		if e.Type != "result" {
			each(e)
			continue
		}
		final = e.Result
		if e.IsError {
			final = "failed: " + final
		}
		a.mu.Lock() // done: Tell now answers that it is not taking messages
		a.running[name] = nil
		p.close()
		a.mu.Unlock()
	}
	if err := p.wait(); err != nil && final == "" {
		final = "failed: " + err.Error()
	}
	report(final)
}

// ZoomAgent gives a subagent's whole chat.
func (c *Chat) ZoomAgent(name string) string {
	b, err := os.ReadFile(filepath.Join(c.dir, "agents", badName.ReplaceAllString(name, "-")+".log"))
	if err != nil {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	return clip(string(b), clipMax)
}
