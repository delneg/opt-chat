package main

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// agentState is the subagent name's state, as the rail shows it.
func agentState(a *Agent, name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.subs[name]; s != nil {
		return s.State
	}
	return ""
}

// workLines are the chat's work messages that start with prefix.
func workLines(c *Chat, prefix string) []string {
	return slices.DeleteFunc(logged(c), func(s string) bool { return !strings.HasPrefix(s, "work: "+prefix) })
}

// The page's tellAgent: the user's text and files reach the subagent as
// content, its log has them as a user step, the chat a work line that
// starts no turn; a resend with the same key is taken once; an unknown
// name fails. Its reply goes to its log alone. agentContext lists what it
// was given.
func TestWebTellAgent(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	a, c := wt.h.agent, wt.c
	a.Spawn("Scout", "look around", "", "", 0, nil)
	waitFor(t, "its report", func() bool { return len(workLines(c, "[Scout] answer")) == 1 })
	waitFor(t, "it at rest", func() bool { return agentState(a, "Scout") == "resting" })
	waitFor(t, "idle", wt.h.idle)
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	resp := wt.do("POST", "/upload", bytes.NewReader(pngBytes(4, 4)), map[string]string{"Cookie": wt.cookie, "X-Name": "a.png"})
	var img attachment
	json.UnmarshalRead(resp.Body, &img)

	for seq := range 2 { // the second as after a reconnect: the same key
		ws.send(map[string]any{"t": "tellAgent", "seq": seq, "key": "page:t1", "name": "Scout", "text": "check this", "files": []attachment{img}})
		ws.next("sent", func(u map[string]any) bool { return u["t"] == "sent" && u["seq"].(float64) == float64(seq) })
	}
	waitFor(t, "its reply", func() bool { return strings.Contains(agentPlain(c.dir, "Scout"), "with 1 images") })
	log := agentPlain(c.dir, "Scout")
	if !strings.Contains(log, "user: check this\n\n<attachment name=\"a.png\" path=") || strings.Count(log, "user: check this") != 1 {
		t.Fatalf("its log: %q", log)
	}
	if !strings.Contains(log, `agent: answer to "The user's message to you, from the page:\ncheck this`) {
		t.Fatalf("what it got: %q", log)
	}
	lines := workLines(c, "[user → Scout]")
	if len(lines) != 1 || lines[0] != "work: [user → Scout] check this\n\nAttached: a.png" {
		t.Fatalf("the chat's line: %q", lines)
	}
	c.mu.Lock()
	var line Msg
	for _, m := range c.msgs {
		if strings.HasPrefix(m.Text, "[user → Scout]") {
			line = m
		}
	}
	c.mu.Unlock()
	if len(line.Images) != 1 {
		t.Fatalf("the line's images: %v", line.Images)
	}
	waitFor(t, "it at rest again", func() bool { return agentState(a, "Scout") == "resting" })
	time.Sleep(200 * time.Millisecond)
	if n := len(workLines(c, "[Scout]")); n != 1 {
		t.Fatalf("%d reports: its reply to the user went to Opt", n)
	}
	if !wt.h.idle() {
		t.Fatal("the user's message to it started a turn")
	}

	ws.send(map[string]any{"t": "tellAgent", "seq": 3, "key": "page:t2", "name": "Nobody", "text": "hi"})
	if u := ws.next("failed", typ("failed")); u["seq"].(float64) != 3 || u["s"] != "Nothing sent: no subagent [Nobody]" {
		t.Fatalf("failed: %v", u)
	}
	ws.send(map[string]any{"t": "tellAgent", "seq": 4, "key": "page:t3", "name": "Scout", "text": "  "})
	ws.next("nothing to send", func(u map[string]any) bool { return u["t"] == "sent" && u["seq"].(float64) == 4 })

	ws.send(map[string]any{"t": "agentContext", "name": "[Scout]"})
	u := ws.next("its context", typ("agentContext"))
	var titles []string
	for _, s := range u["sections"].([]any) {
		titles = append(titles, s.(map[string]any)["title"].(string))
	}
	if u["name"] != "Scout" || !slices.Equal(titles, []string{"Task", "Model", "Files"}) {
		t.Fatalf("its context: %v", u)
	}
	ws.send(map[string]any{"t": "agentContext", "name": "Nobody"})
	ws.next("none", func(u map[string]any) bool { return u["t"] == "notice" && u["s"] == "No subagent [Nobody]." })
}

// The page's compactAgent: a subagent at rest compacts now, and the page
// hears so; one with no call underway can't, and the page hears why.
func TestWebCompactAgent(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	a, c := wt.h.agent, wt.c
	a.Spawn("Scout", "look around", "", "", 0, nil)
	waitFor(t, "it at rest", func() bool { return agentState(a, "Scout") == "resting" })
	ws, err := wt.dial(-1, nil)
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	ws.send(map[string]any{"t": "compactAgent", "name": "[Scout]"})
	ws.next("it compacts", func(u map[string]any) bool {
		return u["t"] == "notice" && u["level"] == "info" && u["s"] == "[Scout] compacts its context now."
	})
	waitFor(t, "its compaction", func() bool { return strings.Contains(agentPlain(c.dir, "Scout"), "compacted: 300000 tokens to 20000") })
	waitFor(t, "it at rest again", func() bool { return agentState(a, "Scout") == "resting" })
	ws.send(map[string]any{"t": "compactAgent", "name": "Nobody"})
	ws.next("no call", func(u map[string]any) bool {
		return u["t"] == "notice" && u["level"] == "warn" && u["s"] == "[Nobody] has no call underway: nothing to compact."
	})
	if n := len(workLines(c, "[Scout]")); n != 1 {
		t.Fatalf("%d reports: the compaction reported", n)
	}
}

// Whose turn reports: one the user's message set going, the subagent at
// rest, answers the user alone; a task's turn reports to Opt, a user's
// message in it or not; and Opt's tell in the user's turn makes it report.
// A user's turn the user stops logs nothing in the chat.
func TestUserTurnReports(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	reported := make(chan string, 8)
	a.report = func(text string) { c.Log("work", text); reported <- text }
	report := func() string {
		t.Helper()
		select {
		case got := <-reported:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	none := func() {
		t.Helper()
		select {
		case got := <-reported:
			t.Fatalf("a report: %q", got)
		case <-time.After(300 * time.Millisecond):
		}
	}
	rec := func(name string) agentRecord {
		r, _ := a.loadRecord(name)
		return r
	}

	// At rest, the user's message: a voice message, its words in the chat's line.
	a.Spawn("Rest", "first", "", "", 0, nil)
	report()
	waitFor(t, "Rest at rest", func() bool { return agentState(a, "Rest") == "resting" })
	voice, err := c.save(strings.NewReader("\x1aE\xdf\xa3\x00 not really audio"), "voice 0:02.webm")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(c.dir, voice.Name+".txt"), []byte("hello there\n"), 0o600)
	if out, ok := a.TellUser("[Rest]", "", []attachment{voice}, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "its reply", func() bool { return strings.Contains(agentPlain(c.dir, "Rest"), "hello there\\n</attachment>") })
	waitFor(t, "Rest at rest again", func() bool { return agentState(a, "Rest") == "resting" })
	none()
	if got := workLines(c, "[user → Rest]"); len(got) != 1 || got[0] != `work: [user → Rest]`+"\n\nAttached: voice 0:02.webm (its words: \"hello there\")" {
		t.Fatalf("the chat's line: %q", got)
	}
	if rec("Rest").User {
		t.Fatal("its record still says the user's turn")
	}
	// Opt's tell after it: a report as ever.
	if got := a.Tell("Rest", "second"); !strings.HasPrefix(got, "Sent: [Rest] is at work") {
		t.Fatal(got)
	}
	if got := report(); !strings.HasPrefix(got, `[Rest] answer to "second"`) {
		t.Fatalf("Opt's tell: %q", got)
	}

	// At work on its task, the user's message: the task's end reports.
	a.Spawn("Work", "wait-for-message", "", "", 0, nil)
	waitFor(t, "Work waiting", func() bool { return strings.Contains(agentPlain(c.dir, "Work"), "agent: waiting") })
	if out, ok := a.TellUser("Work", "banana", nil, false); !ok {
		t.Fatal(out)
	}
	if got := report(); !strings.HasPrefix(got, "[Work] got: "+userHead+"banana") {
		t.Fatalf("the task's report: %q", got)
	}

	// The user's turn, then Opt's tell in it: it reports.
	a.Spawn("Mixed", "first", "", "", 0, nil)
	report()
	waitFor(t, "Mixed at rest", func() bool { return agentState(a, "Mixed") == "resting" })
	if out, ok := a.TellUser("Mixed", "wait-for-message", nil, false); !ok {
		t.Fatal(out)
	}
	waitFor(t, "Mixed waiting", func() bool { return strings.Contains(agentPlain(c.dir, "Mixed"), "agent: waiting") })
	if !rec("Mixed").User {
		t.Fatal("its record does not say the user's turn")
	}
	if got := a.Tell("Mixed", "from opt"); got != "Sent." {
		t.Fatal(got)
	}
	if rec("Mixed").User {
		t.Fatal("Opt's tell left it the user's turn")
	}
	if got := report(); !strings.HasPrefix(got, "[Mixed] got: from opt") {
		t.Fatalf("after Opt's tell: %q", got)
	}

	// The user's turn, stopped by the user: nothing in the chat.
	a.Spawn("Stop", "first", "", "", 0, nil)
	report()
	waitFor(t, "Stop at rest", func() bool { return agentState(a, "Stop") == "resting" })
	a.TellUser("Stop", "wait-for-stop", nil, false)
	waitFor(t, "Stop at work", func() bool { return strings.Contains(agentPlain(c.dir, "Stop"), "agent: working") })
	a.StopAgent("Stop")
	waitFor(t, "Stop halted", func() bool { return agentState(a, "Stop") == "halted" })
	none()
	if got := workLines(c, "[Stop] stopped"); len(got) != 0 {
		t.Fatalf("logged: %q", got)
	}
}

// Sent now, the user's message cuts short the call a subagent at work has
// underway, and it goes on with the message at once, its task still Opt's;
// one at rest takes it as any message.
func TestTellUserNow(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	reported := make(chan string, 2)
	a.report = func(text string) { reported <- text }
	report := func() string {
		select {
		case got := <-reported:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("no report")
			return ""
		}
	}
	a.Spawn("Slow", "slow-task", "", "", 0, nil)
	waitFor(t, "its long call", func() bool { return strings.Contains(agentPlain(c.dir, "Slow"), "sleep 600") })
	if out, ok := a.TellUser("Slow", "hurry", nil, true); !ok || out != "Sent: [Slow] was interrupted to read it now." {
		t.Fatal(out, ok)
	}
	if got := report(); !strings.HasPrefix(got, "[Slow] hurried: "+userHead+"hurry") {
		t.Fatalf("its report: %q", got)
	}
	log := agentPlain(c.dir, "Slow")
	call, told, cut := strings.Index(log, "sleep 600"), strings.Index(log, "user: hurry"), strings.Index(log, "echo: The user doesn't want to proceed") // as Claude Code answers a call cut
	if call < 0 || told < call || cut < told || strings.Count(log, "user: hurry") != 1 {
		t.Fatalf("its log: %q", log)
	}
	if n := strings.Count(strings.Join(fakeFiles(a, ".ctl"), ""), `"interrupt"`); n != 1 {
		t.Fatalf("%d interrupts", n)
	}
	if got := workLines(c, "[user → Slow]"); len(got) != 1 {
		t.Fatalf("the chat's notes: %q", got)
	}

	waitFor(t, "Slow at rest", func() bool { return agentState(a, "Slow") == "resting" })
	if out, ok := a.TellUser("Slow", "hurry again", nil, true); !ok || !strings.HasPrefix(out, "Sent: [Slow] is at work on it") {
		t.Fatal(out, ok)
	}
	waitFor(t, "its answer", func() bool { return strings.Contains(agentPlain(c.dir, "Slow"), "hurried: "+userHead+"hurry again") })
	if n := strings.Count(strings.Join(fakeFiles(a, ".ctl"), ""), `"interrupt"`); n != 1 {
		t.Fatalf("%d interrupts", n)
	}
}

// A subagent cut off by a quit as it answered the user goes on when
// opt-chat starts again, the user's turn still: it reports nothing to Opt.
func TestUserTurnSurvivesRestart(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	a.autoResume = true
	a.report = func(text string) { c.Log("work", text) }
	a.Spawn("Cut", "first", "", "", 0, nil)
	waitFor(t, "Cut at rest", func() bool { return agentState(a, "Cut") == "resting" })
	a.TellUser("Cut", "wait-for-stop", nil, false)
	waitFor(t, "Cut at work", func() bool { return strings.Contains(agentPlain(c.dir, "Cut"), "agent: working") })
	a.quit()
	if r, _ := a.loadRecord("Cut"); !r.Cut || !r.User {
		t.Fatalf("its record: %+v", r)
	}

	b, _ := fakeAgent(t, c) // opt-chat again
	b.autoResume = true
	reported := make(chan string, 1)
	b.report = func(text string) { reported <- text }
	msgs, cut := b.recoverAgents()
	if len(msgs) != 0 || !slices.Equal(cut, []string{"Cut"}) {
		t.Fatalf("recovered: %v %v", msgs, cut)
	}
	before := len(workLines(c, ""))
	b.resumeCut(cut)
	waitFor(t, "its reply", func() bool {
		return strings.Contains(agentPlain(c.dir, "Cut"), "agent: answer to "+`"`+restartUserNote)
	})
	waitFor(t, "Cut at rest", func() bool { return agentState(b, "Cut") == "resting" })
	select {
	case got := <-reported:
		t.Fatalf("a report: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	if after := workLines(c, ""); len(after) != before {
		t.Fatalf("the chat heard of it: %q", after[before:])
	}
}

// agentContext gives what the session file keeps: the first message, and
// after a compaction its summary and the message after it.
func TestAgentContext(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	reported := make(chan string, 1)
	a.report = func(text string) { reported <- text }
	a.Spawn("Ctx", "look around", "", "", 300_000, nil)
	<-reported
	rec, _ := a.loadRecord("Ctx")
	path := sessionPath(rec.Session)
	if path == "" {
		t.Fatal("no session file")
	}
	entry := func(v map[string]any) string { b, _ := json.Marshal(v); return string(b) + "\n" }
	user := func(content any, extra ...any) string {
		e := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}
		for i := 0; i+1 < len(extra); i += 2 {
			e[extra[i].(string)] = extra[i+1]
		}
		return entry(e)
	}
	session := user("<chat>\n0+1|user, a line\n</chat>\n"+viewGoesOn) +
		entry(map[string]any{"type": "assistant", "message": map[string]any{"content": []block{textBlock("ok")}}}) +
		user([]block{textBlock("<chat>\n</chat>You are subagent [Ctx]. Your task:\nlook around")}) +
		user([]block{{Type: "tool_result", ToolID: "t1"}}) +
		user("This session is being continued: the summary", "isCompactSummary", true) +
		user([]block{textBlock("Your context was compacted. Above, the chat's view"), {Type: "image"}})
	if err := os.WriteFile(path, []byte(session), 0o600); err != nil {
		t.Fatal(err)
	}
	a.note("Ctx", func(r *agentRecord) { r.Compacted = true })
	got, ok := a.agentContext("Ctx")
	if !ok {
		t.Fatal("no context")
	}
	sec := map[string]string{}
	var titles []string
	for _, s := range got.Sections {
		sec[s.Title] = s.Text
		titles = append(titles, s.Title)
	}
	if !slices.Equal(titles, []string{"Task", "Model", "First message", "Compacted summary", "After its compaction", "Files"}) {
		t.Fatalf("sections: %q", titles)
	}
	if sec["Task"] != "look around" || !strings.HasPrefix(sec["First message"], "<chat>\n0+1|user, a line") ||
		!strings.HasSuffix(sec["First message"], "Your task:\nlook around") || sec["Compacted summary"] != "This session is being continued: the summary" ||
		sec["After its compaction"] != "Your context was compacted. Above, the chat's view[image]" {
		t.Fatalf("sections: %q", sec)
	}
	for _, want := range []string{"mid\n", "State: resting", "Autocompact: past 300k tokens, between two steps", "Compacted: its next message"} {
		if !strings.Contains(sec["Model"], want) {
			t.Fatalf("model: %q, no %q", sec["Model"], want)
		}
	}
	if !strings.Contains(sec["Files"], "Session: "+rec.Session) || !strings.Contains(sec["Files"], "Session file: "+path) ||
		!strings.Contains(sec["Files"], filepath.Join(c.dir, "agents", "Ctx.log")) {
		t.Fatalf("files: %q", sec["Files"])
	}
	if _, ok := a.agentContext("Nobody"); ok {
		t.Fatal("a context for no subagent")
	}
}

// Each tell in one message says whose it is: Opt's after the user's gets
// its head again.
func TestTellBlocks(t *testing.T) {
	t.Parallel()
	ts := []tell{{text: "a"}, {user: true, text: "b", msg: []block{textBlock("b")}}, {text: "c"}, {text: "d"}}
	got := tellBlocks(ts, "")
	want := "a\n\n" + userHead + "b\n\n" + optHead + "c\n\nd"
	if len(got) != 1 || got[0].Text != want {
		t.Fatalf("got %+v", got)
	}
}
