package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func searchTool(a *Agent, text string, before int) string {
	s, _ := callTool(a, false, "", "search", toolArgs{Text: text, Before: before})
	return s
}

func agentLog(t *testing.T, c *Chat, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.dir, "agents", name+".log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

var agentHit = regexp.MustCompile(`zoom\("(\w+):(\d+)", 1\)`)

// search finds a word that only a subagent's chat holds: after the main
// chat's hits, a line per subagent with the zoom that opens each hit, which
// gives the step that holds it, a step split into parts included.
func TestSearchReachesAgents(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	c.Log("user", "fix the needle bug")
	long := strings.Repeat("y", clipMax+10) + " needle in the second part"
	agentLog(t, c, "Fixer", "task: fix it\n\non: opus medium\n\n"+
		"thinking@1761134400000: a needle in thought is no step\n\n"+
		"tool@1761134400001#toolu_1: Bash {\"command\":\"grep needle\"}\n\n"+
		"echo@1761134400002#toolu_1: "+long+"\n\n"+
		"agent@1761134400003: fixed the NEEDLE\n\n")
	agentLog(t, c, "Other", "task: unrelated\n\nagent@1761134400004: done\n\n")

	got := searchTool(a, "needle", 0)
	main := strings.Index(got, "user: fix the needle bug")
	also := strings.Index(got, "also in [Fixer]: 3 hits")
	if main < 0 || also < main || strings.Contains(got, "[Other]") {
		t.Fatalf("search: %s", got)
	}
	hits := agentHit.FindAllStringSubmatch(got[also:], -1)
	if len(hits) != 3 {
		t.Fatalf("want a zoom for each of 3 hits: %s", got[also:])
	}
	for _, h := range hits {
		if step := zoomTool(a, h[1]+":"+h[2], 1); !strings.Contains(strings.ToLower(step), "needle") {
			t.Errorf("zoom(%q, 1) opens a step without the word: %.120q", h[1]+":"+h[2], step)
		}
	}
	if !strings.Contains(searchTool(a, "unrelated", 0), "also in [Other]: 1 hit") {
		t.Errorf("a word only a subagent holds: %s", searchTool(a, "unrelated", 0))
	}
}

// Many subagents holding the word keep the result small: the newest few
// agents, a few zooms each, and a count of the rest.
func TestSearchAgentsBounded(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	for i := range 40 {
		var b strings.Builder
		b.WriteString("task: find it\n\n")
		for j := range 30 {
			fmt.Fprintf(&b, "agent@%d: needle %d %s\n\n", 1761134400000+j, j, strings.Repeat("z", 2000))
		}
		agentLog(t, c, fmt.Sprintf("A%02d", i), b.String())
	}
	got := searchTool(a, "needle", 0)
	if len(got) > 4000 || !strings.Contains(got, "more subagents") {
		t.Fatalf("search gave %d bytes: %.600s", len(got), got)
	}
}

// With -agent-memory=false a search is the main chat's alone.
func TestSearchAgentsOff(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	c.Log("user", "fix the needle bug")
	agentLog(t, c, "Fixer", "task: fix it\n\nagent@1761134400003: fixed the needle\n\n")
	if got := searchTool(a, "needle", 0); strings.Contains(got, "also in") || got != c.Search("needle", 0) {
		t.Fatalf("search with memory off: %s", got)
	}
	for _, mem := range []bool{true, false} {
		a.mems = nil
		if mem {
			a.mems = newAgentMems(c, 1)
		}
		var desc string
		for _, tl := range a.mcpList() {
			if tl["name"] == "search" {
				desc = tl["description"].(string)
			}
		}
		prompt := systemPrompt("", "", false, mem)
		if strings.Contains(desc, "subagent") != mem || strings.Contains(prompt, "subagent whose chat holds it") != mem {
			t.Errorf("memory %v: search says %q; the prompt names subagents' chats: %v", mem, desc, !mem)
		}
	}
}

// A report's line ends with the zoom that opens the subagent's chat, so the
// view keeps the way to it whatever the summarizer wrote; with
// -agent-memory=false a report ends "Full chat:" and its line is the
// summarizer's.
func TestReportLineKeepsChatRef(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 300)
	body := "[Fixer] " + strings.Repeat("did a thing. ", 60)
	c.Log("work", body+"\n\nRan on opus medium. Its chat: zoom(\"Fixer\")")
	c.Log("work", body+"\n\nRan on opus medium. Full chat: zoom(\"Fixer\")")
	waitIdle(t, c)
	c.mu.Lock()
	on, off := c.tree[key{0, 0}], c.tree[key{0, 1}]
	c.mu.Unlock()
	if !strings.HasSuffix(on, ` zoom("Fixer")`) {
		t.Errorf("report line: %q", on)
	}
	if strings.Contains(off, "zoom") {
		t.Errorf("report line with memory off: %q", off)
	}
}

// A search while a step is half written waits for the write, as a zoom
// does: it never counts the step's halves, split at a blank line in its
// text, as two steps, which would point its zooms at the wrong ids.
func TestSearchAgentsTornWrite(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a := memAgent(c)
	os.MkdirAll(filepath.Join(c.dir, "agents"), 0o700)
	a.agentWrite("W", "task", "write steps")
	text := "first needle\n\nsecond needle"
	b := []byte(stepHead(step{Kind: "echo", Text: text, At: nowMs()}) + text + "\n\n")
	cut := bytes.Index(b, []byte("\n\nsecond")) + 2
	am := a.mems.lock("W") // as appendStep holds it over its write
	if err := appendRaw(a.agentLog("W"), b[:cut]); err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() { done <- searchTool(a, "needle", 0) }()
	select {
	case got := <-done:
		am.mu.Unlock()
		t.Fatalf("search read a half-written step: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
	if err := appendRaw(a.agentLog("W"), b[cut:]); err != nil {
		t.Fatal(err)
	}
	am.mu.Unlock()
	if got := <-done; !strings.Contains(got, "also in [W]: 1 hit") || !strings.Contains(got, `zoom("W:1", 1)`) {
		t.Fatalf("search: %s", got)
	}
}
