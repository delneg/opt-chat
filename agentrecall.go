package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

const (
	recallAgentMax = 3   // subagents' steps a recall adds at most
	recallAgentCut = 600 // bytes of a step that recall shows
)

// Recall finds the messages before message before that best share the
// words of text, as Chat.shared ranks them, then the subagents' steps
// that do, as a block for a turn's input; "" when none does.
func (a *Agent) Recall(text string, before, limit int) string {
	lines := a.chat.shared(text, before, limit)
	agents := a.recallAgents(text, limit)
	if lines == "" && agents == "" {
		return ""
	}
	how := ""
	if agents != "" {
		how = "Lines that start [Name] are steps of a subagent's chat, each\n" +
			fmt.Sprintf("cut to %d bytes and ending with the zoom that opens it whole.\n", recallAgentCut)
	}
	return fmt.Sprintf("[opt-chat] Recall: messages from the log that share this message's rarest words,\n"+
		"found by words alone, so some may be beside the point; each cut to %d bytes\n"+
		"around its rarest word: zoom(id, 1) opens one whole.\n%s<recall>\n", recallCut, how) + lines + agents + "</recall>\n\n"
}

// recallAgents is the few steps of all the subagents' chats that best
// share the words of text, ranked together as recall ranks the main
// chat's, a line each: "" when none does, or with -agent-memory=false.
// The main chat holds a subagent's task and report already, so those are
// left out, and so are its own lookups. With a few lines, short ones, the
// turn learns that a subagent's chat holds the answer, and where, without
// a search.
func (a *Agent) recallAgents(text string, limit int) string {
	terms := recallTerms(text)
	if a.mems == nil || len(terms) == 0 || limit <= 0 {
		return ""
	}
	logs, _ := filepath.Glob(filepath.Join(a.chat.dir, "agents", "*.log"))
	type from struct {
		name string
		id   int
	}
	var msgs []Msg
	var src []from
	for _, path := range logs {
		name := strings.TrimSuffix(filepath.Base(path), ".log")
		steps, _, ok := a.agentLogged(path)
		if !ok {
			continue
		}
		lookups := lookupRefs(steps)
		report := len(steps) // its report, a step in parts maybe, ends its chat
		for report > 0 && steps[report-1].Kind == "agent" {
			report--
		}
		for i, m := range steps[:report] {
			if m.Kind != "task" && !lookups[m.Ref] {
				msgs, src = append(msgs, m), append(src, from{name, i})
			}
		}
	}
	a.mems.closeIdle("")
	hits, weight := rankShared(msgs, terms, func(Msg) bool { return false }, func(h []recallHit) []recallHit { return h })
	var b strings.Builder
	for _, h := range hits[:min(recallAgentMax, limit, len(hits))] {
		m, f := msgs[h.i], src[h.i]
		date := ""
		if !m.Date.IsZero() && m.Date.UnixMilli() > 0 {
			date = ", " + m.Date.Local().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "[%s]:%d+1|%s%s: %s zoom(%q, 1)\n", f.name, f.id, m.Kind, date,
			recallTag.Replace(flat(excerpt(m.Text, terms[h.rarest(weight)], recallAgentCut))), fmt.Sprintf("%s:%d", f.name, f.id))
	}
	return b.String()
}
