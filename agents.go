package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Agents run in the background, so a long task never blocks the chat. An
// agent is a fresh call whose first message is the view, then its task. Its
// own steps stay in its own log, agents/<Name>.log, out of the chat; its
// final reply comes back to the chat as one work message, "[Name] report".

var badName = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// agentName is s as a subagent's name, the same however it is written:
// "[My agent]" is My-agent.
func agentName(s string) string {
	return strings.Trim(badName.ReplaceAllString(strings.Trim(s, "[] "), "-"), "-")
}

func (a *Agent) agentLog(name string) string { return filepath.Join(a.chat.dir, "agents", name+".log") }

var efforts = []string{"low", "medium", "high", "xhigh", "max"}

func validEffort(e string) bool { return slices.Contains(efforts, e) }

func joinEfforts() string { return strings.Join(efforts, ", ") }

// Spawn starts an agent and returns at once. An empty effort is
// -agent-effort; an empty model, -agent-model.
func (a *Agent) Spawn(name, task, effort, model string, window int) string {
	if strings.TrimSpace(task) == "" {
		return "No task given."
	}
	if effort != "" && !slices.Contains(efforts, effort) {
		return "effort must be one of " + strings.Join(efforts, ", ") + ", or left out."
	}
	if model != "" && !modelName.MatchString(model) {
		return "model is one word: opus, fable, sonnet, haiku, or a full model name; or left out."
	}
	if window != 0 && (window < windowMin || window > windowMax) {
		return fmt.Sprintf("autocompact must be from %d to %d tokens, or left out.", windowMin, windowMax)
	}
	if effort == "" {
		effort = a.aeffort
	}
	if model == "" {
		model = a.amodel
	}
	name = agentName(name)
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
		a.runAgent(name, task, effort, model, window)
	}()
	return fmt.Sprintf("Started [%s] in the background, on %s. Its report will come as a message starting \"[%s]\".", name, model, name)
}

// Tell hands a running agent a message, between its tool calls.
func (a *Agent) Tell(name, text string) string {
	name = agentName(name)
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.running[name]
	if !ok {
		return fmt.Sprintf("No subagent [%s] is taking messages.", name)
	}
	if p == nil {
		return fmt.Sprintf("[%s] is starting: tell it again in a moment.", name)
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

func (a *Agent) runAgent(name, task, effort, model string, window int) {
	report := func(text string) {
		a.mu.Lock()
		delete(a.running, name)
		a.mu.Unlock()
		a.chat.hub.notice("info", "[%s] done", name)
		if !strings.HasPrefix(text, "["+name+"]") { // as the prompt asks of a report
			text = "[" + name + "] " + text
		}
		a.report(fmt.Sprintf("%s\n\nRan on %s. Full chat: zoom(%q)", text, strings.TrimSpace(model+" "+effort), name)) // as UniiChat ends a report
	}
	if err := os.MkdirAll(filepath.Join(a.chat.dir, "agents"), 0o700); err != nil {
		report("failed: " + err.Error())
		return
	}
	a.agentWrite(name, "task", task)
	a.chat.hub.notice("info", "[%s] started on %s", name, strings.TrimSpace(model+" "+effort))
	a.agentWrite(name, "on", strings.TrimSpace(model+" "+effort))
	c := a.chat
	failed := func(err error) string {
		if a.life.Err() != nil {
			return "stopped: opt-chat quit before this subagent finished."
		}
		return "failed: " + err.Error()
	}
	if err := c.settle(a.life, func(int, int) {}); err != nil {
		report(failed(err))
		return
	}
	c.mu.Lock()
	lines := c.callView()
	c.mu.Unlock()
	a.mu.Lock()
	if window == 0 {
		window = a.agentWindow
	}
	a.mu.Unlock()
	p, err := a.start(context.Background(), model, effort, roleAgent, window)
	if err != nil {
		report(failed(err))
		return
	}
	p.who = name // before its first message: its questions say who asks
	acking := true
	each := func(e event) {
		if e.Parent != nil {
			return
		}
		if e.Type == "system" && e.Subtype == "compact_boundary" && !acking {
			// Claude Code's summary of the call stands in for the view and the
			// task: they come again, as they are now.
			a.agentWrite(name, "compacted", fmt.Sprintf("%d tokens to %d", e.Compact.Pre, e.Compact.Post))
			a.chat.hub.notice("info", "[%s]'s context was compacted, %dk tokens to %dk: it gets the view and its task again", name, e.Compact.Pre/1000, e.Compact.Post/1000)
			c.mu.Lock()
			lines := c.callView()
			c.mu.Unlock()
			_, again := viewMessages(lines, []block{textBlock(fmt.Sprintf("Claude Code compacted your context. Above, the chat's view as it is now; "+
				"you are still subagent [%s], and your task is:\n%s\nGo on from where you were.", name, task))})
			p.send(again...)
			return
		}
		for _, b := range e.blocks() {
			switch {
			case e.Type == "assistant" && b.Type == "text" && !acking:
				a.agentWrite(name, "agent", b.Text)
			case e.Type == "assistant" && b.Type == "tool_use":
				a.agentWrite(name, "tool", toolCall(b))
			case e.Type == "user" && b.Type == "tool_result":
				out, _ := resultContent(b.Content)
				a.agentWrite(name, "echo", clip(out, clipMax))
			}
		}
	}
	var u usage
	a.mcpSettle(p)
	first, second := viewMessages(lines, []block{textBlock(fmt.Sprintf("You are subagent [%s]. Your task:\n%s", name, task))})
	if err := a.ack(p, first, &u, each); err != nil {
		p.wait()
		report(failed(err))
		return
	}
	acking = false
	a.mu.Lock()
	p.send(second...)
	a.running[name] = p
	a.mu.Unlock()
	final, replied := "", false
	for {
		e, ok := p.next()
		if !ok {
			break
		}
		if e.Type != "result" {
			each(e)
			continue
		}
		final, replied = e.Result, true
		if e.IsError {
			final = "failed: " + final
		}
		a.mu.Lock() // done: Tell now answers that it is not taking messages
		delete(a.running, name)
		p.close()
		a.mu.Unlock()
	}
	if err := p.wait(); !replied {
		final = failed(fmt.Errorf("no reply: %v", err))
	}
	report(final)
}

// ZoomAgent gives a subagent's whole chat.
func (c *Chat) ZoomAgent(name string) string {
	name = agentName(name)
	b, err := os.ReadFile(filepath.Join(c.dir, "agents", name+".log"))
	if name == "" || err != nil {
		return fmt.Sprintf("No subagent [%s].", name)
	}
	return string(b)
}
