package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Side questions, after Claude Code's /btw
// (https://code.claude.com/docs/en/interactive-mode#side-questions-with-btw):
// "/btw question" is answered beside the chat, never in it. The call is a
// turn's (the same prompt, tools, mode and view, so it reads the turns'
// cache) and may look things up in the chat, but neither the question nor
// the answer joins the log: no turn ever sees them, and compactions never
// summarize them. They are kept in btw/, by day, and a page shows each
// after the message it followed.

// Aside is one side question and its answer.
type Aside struct {
	After  int       `json:"after"`           // the log's last message when it was asked; -1: none
	Q      string    `json:"q"`               // as the log keeps a message: its attachments' marks in it
	A      string    `json:"a,omitzero"`      // the answer; empty while it is written
	Err    string    `json:"err,omitzero"`    // or why there is none
	Date   time.Time `json:"date"`            // when it was asked, which names it
	Images []string  `json:"images,omitzero"` // as a Msg's
}

const (
	btwEarlier = 20             // earlier side questions a new one sees, as in Claude Code...
	btwSince   = 12 * time.Hour // ...asked this recently: opt-chat never clears them
)

const btwFrame = "[opt-chat] A side question, asked with /btw. Answer it briefly, from the view and from what zoom, search and date find in the chat. " +
	"Neither it nor your answer joins the chat: no turn will ever see them, so start no work and change nothing. " +
	"spawn, tell and any tool call that needs the user's permission are refused here.\n\n"

// btwQuestion is text's question when text is a /btw command.
func btwQuestion(text string) (string, bool) {
	m := commandWord.FindStringSubmatch(text)
	if m == nil || !strings.EqualFold(m[1], "btw") {
		return "", false
	}
	return m[2], true
}

// aside answers a /btw message beside the chat, in the background; false:
// m is for the chat.
func (h *harness) aside(m message) bool {
	q, ok := btwQuestion(m.text)
	if !ok || m.kind != "user" {
		return false
	}
	m.text = q
	go h.agent.Btw(m)
	return true
}

// Btw answers m, a side question, and keeps both: see Aside.
func (a *Agent) Btw(m message) {
	c := a.chat
	c.mu.Lock()
	lines := c.callView()
	at := Aside{After: len(c.msgs) - 1, Q: m.full(), Date: time.Now(), Images: m.images}
	earlier := c.recentAsides(at.Date)
	i := len(c.asides)
	c.asides = append(c.asides, at)
	c.hub.send(asideUpdate{T: "aside", Aside: at})
	c.mu.Unlock()
	answer, err := a.btw(lines, earlier, m, at.After+1)
	if err != nil {
		at.Err = err.Error()
	}
	at.A = answer
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asides[i] = at
	c.appendDay("btw", at)
	c.hub.send(asideUpdate{T: "aside", Aside: at})
}

// recentAsides are the side questions answered lately, oldest first, for
// the next one to see. Caller holds c.mu.
func (c *Chat) recentAsides(now time.Time) []Aside {
	var out []Aside
	for j := len(c.asides) - 1; j >= 0 && len(out) < btwEarlier; j-- {
		if s := c.asides[j]; s.A != "" && now.Sub(s.Date) <= btwSince {
			out = append([]Aside{s}, out...)
		}
	}
	return out
}

// btw runs the call: the view, then whatever the call needs, then the
// question. before: where the log was when it was asked, for Recall.
func (a *Agent) btw(lines []string, earlier []Aside, m message, before int) (string, error) {
	c := a.chat
	input := a.atWork()
	if r := c.Recall(unframed(m.full()), before, a.recall); r != "" {
		input = append(input, textBlock(r))
	}
	if len(earlier) > 0 {
		var b strings.Builder
		b.WriteString("[opt-chat] The user's earlier side questions, and your answers, which the chat never saw either:\n<btw>\n")
		for _, s := range earlier {
			fmt.Fprintf(&b, "%s, after message %d:\nQ: %s\nA: %s\n\n", s.Date.Format(time.DateTime), s.After, unframed(s.Q), s.A)
		}
		input = append(input, textBlock(strings.TrimSuffix(b.String(), "\n")+"</btw>\n\n"))
	}
	input = append(append(input, textBlock(btwFrame)), c.content(m)...)
	a.mu.Lock()
	model, effort := a.model, a.effort
	a.mu.Unlock()
	p, err := a.start(context.Background(), model, effort, roleBtw)
	if err != nil {
		return "", err
	}
	defer p.wait()
	a.mcpSettle(p)
	var u usage
	first, second := viewMessages(lines, input)
	if err := a.ack(p, first, &u, nil); err != nil {
		return "", err
	}
	p.send(second...)
	for {
		e, ok := p.next()
		if !ok {
			return "", p.exited()
		}
		if e.Type != "result" {
			continue
		}
		answer := strings.TrimSpace(e.Result)
		switch {
		case e.IsError:
			return "", errors.New(e.failure())
		case answer == "":
			return "", errors.New("the answer came back empty")
		}
		return answer, nil
	}
}
