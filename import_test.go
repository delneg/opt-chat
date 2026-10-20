package main

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func jsonl(t *testing.T, path string, lines ...any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		b, _ := json.Marshal(l)
		f.Write(append(b, '\n'))
	}
}

type obj = map[string]any

func claudeLine(typ, at string, content any, extra ...any) obj {
	l := obj{"type": typ, "timestamp": at, "cwd": "/work", "message": obj{"role": typ, "content": content}}
	for i := 0; i+1 < len(extra); i += 2 {
		l[extra[i].(string)] = extra[i+1]
	}
	return l
}

// logged is the chat's messages as "kind: text" lines.
func logged(c *Chat) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		out = append(out, m.Kind+": "+m.Text)
	}
	return out
}

func importInto(t *testing.T, c *Chat, path string, o importOpts, answer string) string {
	t.Helper()
	var out strings.Builder
	if err := importChats(c, path, o, strings.NewReader(answer), &out); err != nil && err != errDeclined {
		t.Fatal(err)
	}
	return out.String()
}

// A Claude Code session: its words and images come in at their dates, under
// a note; Claude Code's own lines, thinking, other agents' steps and (unless
// asked for) tools stay out; a second import adds only what is new.
func TestImportClaudeCode(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, ".claude", "projects", "-work", "s1.jsonl")
	img := base64.StdEncoding.EncodeToString([]byte(pngHead))
	jsonl(t, file,
		obj{"type": "summary", "summary": "old"},
		claudeLine("user", "2026-09-01T10:00:00Z", "Fix the build"),
		claudeLine("user", "2026-09-01T10:00:01Z", "Caveat: ...", "isMeta", true),
		claudeLine("assistant", "2026-09-01T10:00:02Z", []obj{
			{"type": "thinking", "thinking": "hmm"},
			{"type": "text", "text": "Looking."},
			{"type": "tool_use", "id": "t1", "name": "Bash", "input": obj{"command": "make"}},
		}),
		claudeLine("user", "2026-09-01T10:00:03Z", []obj{{"type": "tool_result", "tool_use_id": "t1", "content": "ok " + strings.Repeat("x", 40_000)}}),
		claudeLine("assistant", "2026-09-01T10:00:04Z", "a step of another agent", "isSidechain", true),
		claudeLine("user", "2026-09-01T10:00:05Z", "<command-name>/clear</command-name>"),
		claudeLine("user", "2026-09-01T10:00:06Z", []obj{{"type": "text", "text": "[Request interrupted by user]"}}),
		claudeLine("user", "2026-09-01T10:00:06Z", "/compact"),
		claudeLine("user", "2026-09-01T10:00:06Z", "<bash-input>pwd</bash-input>"),
		claudeLine("user", "2026-09-01T10:00:06Z", "<bash-stdout>/work</bash-stdout><bash-stderr></bash-stderr>"),
		claudeLine("user", "2026-09-01T10:00:07Z", []obj{
			{"type": "text", "text": "See [Image #1]"},
			{"type": "image", "source": obj{"type": "base64", "media_type": "image/png", "data": img}},
		}),
		// Typed mid-turn, then handed back by an Esc and sent again: once.
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:07Z", "attachment": obj{"type": "queued_command", "commandMode": "prompt", "prompt": "and the docs"}},
		claudeLine("user", "2026-09-01T10:00:07Z", []obj{{"type": "text", "text": "[Request interrupted by user]"}}),
		claudeLine("user", "2026-09-01T10:00:07Z", "and the docs"),
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:08Z", "attachment": obj{"type": "queued_command", "commandMode": "prompt", "prompt": []obj{{"type": "text", "text": "and the tests"}}}},
		// Claude Code's own notices, queued the same way.
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:08Z", "attachment": obj{"type": "queued_command", "commandMode": "task-notification", "prompt": "<task-notification>done</task-notification>"}},
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:08Z", "attachment": obj{"type": "queued_command", "commandMode": "prompt", "prompt": "an agent's report", "origin": obj{"kind": "agent"}}},
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:08Z", "attachment": obj{"type": "queued_command", "commandMode": "prompt", "prompt": "a reminder", "isMeta": true}},
	)
	jsonl(t, filepath.Join(home, ".claude", "projects", "-work", "s1", "subagents", "a.jsonl"), claudeLine("user", "2026-09-01T10:00:00Z", "sub"))
	jsonl(t, filepath.Join(home, ".claude", "other.jsonl"), claudeLine("user", "2026-09-01T10:00:00Z", "not a session"))
	jsonl(t, filepath.Join(home, ".claude", "projects", "-bots", "b.jsonl"), claudeLine("user", "2026-09-01T11:00:00Z", "a bot's prompt", "cwd", "/bots/run"))

	c, _ := fakeChat(t, t.TempDir(), 100)
	empty := filepath.Join(home, "empty")
	os.Mkdir(empty, 0o700)
	if err := importChats(c, empty, importOpts{}, strings.NewReader("y\n"), io.Discard); err == nil || err.Error() != "no chats in "+empty {
		t.Fatalf("a directory with no session: %v", err)
	}
	if out := importInto(t, c, filepath.Join(home, ".claude"), importOpts{}, "n\n"); !strings.Contains(out, "6 messages (0.0 MB) from 2 sessions, ") ||
		!strings.Contains(out, "-import-tools adds them") || len(logged(c)) != 0 {
		t.Fatalf("declined: %q, logged %q", out, logged(c))
	}
	out := importInto(t, c, filepath.Join(home, ".claude"), importOpts{skip: []string{"", "/bots", "s1"}}, "y\n") // s1 is an id, not a directory
	if !strings.HasSuffix(out, "\rWriting to the log: 1 of 1 session\nImported.\n") {
		t.Fatalf("accepted: %q", out)
	}
	want := []string{
		"note: Claude Code session s1 in /work, from 2026-09-01 10:00:00 UTC (imported)",
		"user: Fix the build",
		"ai: Looking.",
		"user: See [Image #1]",
		"user: and the docs",
		"user: and the tests",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
	if m := c.msgs[3]; len(m.Images) != 1 || !m.Date.Equal(time.Date(2026, 9, 1, 10, 0, 7, 0, time.UTC)) {
		t.Fatalf("the image message: %+v", m)
	}
	if i := slices.IndexFunc(c.msgs, func(m Msg) bool { return !m.Imported }); i >= 0 {
		t.Fatalf("message %d not marked imported", i)
	}
	if out := importInto(t, c, filepath.Join(home, ".claude"), importOpts{skip: []string{"/bots"}}, "y\n"); !strings.HasPrefix(out, "Nothing new in the 2 sessions found") {
		t.Fatalf("a second import: %q", out)
	}

	// The session goes on: the next import adds the rest, here with tools.
	c.Log("user", "a live message") // after the import: never counted as the session's
	if c.msgs[len(c.msgs)-1].Imported {
		t.Fatal("a live message marked imported")
	}
	jsonl(t, file,
		claudeLine("assistant", "2026-09-02T09:00:00Z", []obj{{"type": "text", "text": "Done."}, {"type": "tool_use", "name": "Read", "input": obj{"file_path": "a"}}}),
		claudeLine("user", "2026-09-02T09:00:01Z", []obj{{"type": "tool_result", "content": "contents of " + strings.Repeat("y", 40_000)}}),
	)
	importInto(t, c, file, importOpts{tools: true}, "y\n")
	got := logged(c)[7:]
	if len(got) != 4 || got[0] != want[0] || got[1] != "ai: Done." || got[2] != `tool: Read {"file_path":"a"}` ||
		!strings.HasPrefix(got[3], "echo: contents of ") || !strings.Contains(got[3], " characters cut ...]") {
		t.Fatalf("the rest: %q", got)
	}
	if out := importInto(t, c, file, importOpts{since: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), tools: true}, "y\n"); !strings.HasPrefix(out, "Nothing new") {
		t.Fatalf("-import-since past the end: %q", out)
	}
}

// Claude Code's history.jsonl gives the user's words of sessions whose
// transcripts are gone, without commands, with pastes in full. A forked
// session's copied lines come in once, also when its source came in an
// earlier import, and a session imported before is the same session when
// only its history is left.
func TestImportHistory(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".claude")
	ms := func(at string) int64 { d, _ := time.Parse(time.RFC3339, at); return d.UnixMilli() }
	typed := func(session, project, at, text string, pasted ...string) obj {
		e := obj{"display": text, "pastedContents": obj{}, "timestamp": ms(at), "project": project, "sessionId": session}
		for i, p := range pasted {
			e["pastedContents"].(obj)[strconv.Itoa(i+1)] = obj{"id": i + 1, "type": "text", "content": p}
		}
		return e
	}
	s1 := filepath.Join(home, "projects", "-work", "s1.jsonl")
	jsonl(t, s1,
		claudeLine("user", "2026-09-01T10:00:00Z", "Plan it", "uuid", "u1"),
		claudeLine("assistant", "2026-09-01T10:00:01Z", "Planned.", "uuid", "u2"),
	)
	history := filepath.Join(home, "history.jsonl")
	jsonl(t, history,
		obj{"display": "an old prompt", "pastedContents": obj{}, "timestamp": ms("2025-10-14T09:00:00Z"), "project": "/old"},
		typed("s3", "/docs", "2026-08-01T08:00:00Z", "/clear"),
		typed("s3", "/docs", "2026-08-01T08:00:01Z", "!ls"),
		typed("s3", "/docs", "2026-08-01T08:00:02Z", "/review this pull request carefully"),
		typed("s3", "/docs", "2026-08-01T08:00:03Z", "Summarize [Pasted text #1 +2 lines] and [Pasted text #2]", "a\nb"),
		typed("s3", "/docs", "2026-08-01T08:00:04Z", "/model opus"),
		typed("s3", "/docs", "2026-08-01T08:00:05Z", "/Users/me/shot.png what is it"),
		typed("s1", "/work", "2026-09-01T10:00:00Z", "Plan it"), // in its transcript
		typed("s4", "/x", "2026-09-01T11:00:00Z", "/clear"),     // not a chat
	)
	c, _ := fakeChat(t, t.TempDir(), 100)
	if out := importInto(t, c, home, importOpts{}, "y\n"); !strings.Contains(out, "6 messages (0.0 MB) from 3 sessions, ") {
		t.Fatalf("the plan: %q", out)
	}
	want := []string{
		"note: Claude Code session in /old, your messages only, from 2025-10-14 09:00:00 UTC (imported)",
		"user: an old prompt",
		"note: Claude Code session s3 in /docs, your messages only, from 2026-08-01 08:00:02 UTC (imported)",
		"user: /review this pull request carefully",
		"user: Summarize a\nb and [Pasted text #2]",
		"user: /Users/me/shot.png what is it",
		"note: Claude Code session s1 in /work, from 2026-09-01 10:00:00 UTC (imported)",
		"user: Plan it",
		"ai: Planned.",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
	if m := c.msgs[4]; !m.Date.Equal(time.Date(2026, 8, 1, 8, 0, 3, 0, time.UTC)) {
		t.Fatalf("a prompt's date: %v", m.Date)
	}

	// s2 forks s1: it starts with a copy of s1's reply.
	jsonl(t, filepath.Join(home, "projects", "-work", "s2.jsonl"),
		claudeLine("assistant", "2026-09-01T10:00:01Z", "Planned.", "uuid", "u2"),
		claudeLine("user", "2026-09-02T09:00:00Z", "Go on", "uuid", "u3"),
	)
	importInto(t, c, home, importOpts{}, "y\n")
	if got := logged(c)[len(want):]; !slices.Equal(got, []string{"note: Claude Code session s2 in /work, from 2026-09-01 10:00:01 UTC (imported)", "user: Go on"}) {
		t.Fatalf("the fork: %q", got)
	}

	// s1's transcript is deleted; its history goes on.
	os.Remove(s1)
	jsonl(t, history, typed("s1", "/work", "2026-09-03T12:00:00Z", "And test it"))
	importInto(t, c, home, importOpts{}, "y\n")
	if got := logged(c)[len(want)+2:]; !slices.Equal(got, []string{"note: Claude Code session s1 in /work, your messages only, from 2026-09-01 10:00:00 UTC (imported)", "user: And test it"}) {
		t.Fatalf("the rest from the history: %q", got)
	}
}

// Codex sessions: injected context and subagents' sessions stay out.
func TestImportCodex(t *testing.T) {
	home := t.TempDir()
	item := func(at string, p obj) obj { return obj{"type": "response_item", "timestamp": at, "payload": p} }
	jsonl(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "01", "rollout-1.jsonl"),
		obj{"type": "session_meta", "timestamp": "2026-09-01T08:00:00Z", "payload": obj{"id": "c1", "cwd": "/code", "source": "cli"}},
		item("2026-09-01T08:00:01Z", obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "<environment_context>x</environment_context>"}}}),
		item("2026-09-01T08:00:02Z", obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "list files"}}}),
		item("2026-09-01T08:00:03Z", obj{"type": "function_call", "name": "shell", "arguments": `{"command":["ls"]}`, "call_id": "call_1"}),
		item("2026-09-01T08:00:04Z", obj{"type": "function_call_output", "output": "a b", "call_id": "call_1"}),
		item("2026-09-01T08:00:05Z", obj{"type": "message", "role": "assistant", "content": []obj{{"type": "output_text", "text": "Two files."}}}),
		item("2026-09-01T08:00:06Z", obj{"type": "reasoning", "summary": []obj{}}),
	)
	jsonl(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "01", "rollout-2.jsonl"),
		obj{"type": "session_meta", "timestamp": "2026-09-01T09:00:00Z", "payload": obj{"cwd": "/code", "source": obj{"subagent": "review"}}},
		item("2026-09-01T09:00:01Z", obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "review it"}}}),
	)
	c, _ := fakeChat(t, t.TempDir(), 100)
	importInto(t, c, filepath.Join(home, ".codex"), importOpts{tools: true}, "yes\n")
	want := []string{
		"note: Codex session c1 in /code, from 2026-09-01 08:00:02 UTC (imported)",
		"user: list files",
		`tool: shell {"command":["ls"]}`,
		"echo: a b",
		"ai: Two files.",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
	if c.msgs[2].Ref != "call_1" || c.msgs[3].Ref != "call_1" { // a result's compaction finds its call
		t.Fatalf("refs %q, %q", c.msgs[2].Ref, c.msgs[3].Ref)
	}
}

// Claude Code's calls made at once and their results keep their ids, so
// each result's compaction finds its call. A message typed mid-turn and
// handed back by an Esc is imported once: dropped when the next prompt
// sends it again, kept when it does not.
func TestImportQueuedAndRefs(t *testing.T) {
	home := t.TempDir()
	queued := func(at, text string) obj {
		return obj{"type": "attachment", "timestamp": at, "attachment": obj{"type": "queued_command", "commandMode": "prompt", "prompt": text, "origin": obj{"kind": "human"}}}
	}
	esc := func(at string) obj {
		return claudeLine("user", at, []obj{{"type": "text", "text": "[Request interrupted by user]"}})
	}
	jsonl(t, filepath.Join(home, ".claude", "projects", "-work", "q.jsonl"),
		claudeLine("user", "2026-09-01T10:00:00Z", "Read both"),
		claudeLine("assistant", "2026-09-01T10:00:01Z", []obj{
			{"type": "tool_use", "id": "ta", "name": "Read", "input": obj{"file_path": "a"}},
			{"type": "tool_use", "id": "tb", "name": "Read", "input": obj{"file_path": "b"}},
		}),
		claudeLine("user", "2026-09-01T10:00:02Z", []obj{{"type": "tool_result", "tool_use_id": "ta", "content": "A"}, {"type": "tool_result", "tool_use_id": "tb", "content": "B"}}),
		queued("2026-09-01T10:00:03Z", "use the staging db"),
		queued("2026-09-01T10:00:03Z", "and be quick"),
		esc("2026-09-01T10:00:04Z"),
		claudeLine("user", "2026-09-01T10:00:05Z", "and be quick, please"), // sends the second again, not the first
		claudeLine("assistant", "2026-09-01T10:00:06Z", "Using staging."),
	)
	c, _ := fakeChat(t, t.TempDir(), 100)
	importInto(t, c, filepath.Join(home, ".claude"), importOpts{tools: true}, "y\n")
	want := []string{
		"user: Read both",
		`tool: Read {"file_path":"a"}`,
		`tool: Read {"file_path":"b"}`,
		"echo: A",
		"echo: B",
		"user: use the staging db",
		"user: and be quick, please",
		"ai: Using staging.",
	}
	if got := logged(c)[1:]; !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
	if refs := []string{c.msgs[2].Ref, c.msgs[3].Ref, c.msgs[4].Ref, c.msgs[5].Ref}; !slices.Equal(refs, []string{"ta", "tb", "ta", "tb"}) {
		t.Fatalf("refs %q", refs)
	}
}

// Claude's and ChatGPT's exports: Claude's from its .zip; ChatGPT's along the
// branch the user last saw, without hidden messages or thoughts.
func TestImportExports(t *testing.T) {
	dir := t.TempDir()
	claude := []obj{{
		"name": "Trip", "created_at": "2026-05-01T12:00:00.000000Z",
		"chat_messages": []obj{
			{"sender": "human", "text": "Plan a trip", "content": []obj{{"type": "text", "text": "Plan a trip"}}, "created_at": "2026-05-01T12:00:00Z",
				"files": []obj{{"file_name": "map.pdf"}}},
			{"sender": "assistant", "text": "Sure.", "created_at": "2026-05-01T12:00:05Z"},
		},
	}}
	zipPath := filepath.Join(dir, "claude.zip")
	f, _ := os.Create(zipPath)
	z := zip.NewWriter(f)
	w, _ := z.Create("data/conversations.json")
	b, _ := json.Marshal(claude)
	w.Write(b)
	z.Close()
	f.Close()

	msg := func(role, recipient string, at float64, parts ...any) obj {
		return obj{"author": obj{"role": role}, "recipient": recipient, "create_time": at, "content": obj{"content_type": "text", "parts": parts}}
	}
	gpt := []obj{{
		"title": "Code", "create_time": 1.7e9, "current_node": "d",
		"mapping": obj{
			"root": obj{"parent": nil, "message": nil},
			"a":    obj{"parent": "root", "message": msg("user", "all", 1.7e9+1, "write a poem")},
			"old":  obj{"parent": "a", "message": msg("assistant", "all", 1.7e9+2, "a branch left behind")},
			"b":    obj{"parent": "a", "message": obj{"author": obj{"role": "assistant"}, "content": obj{"content_type": "thoughts", "thoughts": []any{}}}},
			"c":    obj{"parent": "b", "message": msg("assistant", "python", 1.7e9+3, "print(1)")},
			"c2":   obj{"parent": "c", "message": msg("tool", "all", 1.7e9+4, "1")},
			"hid":  obj{"parent": "c2", "message": obj{"author": obj{"role": "system"}, "content": obj{"content_type": "text", "parts": []any{"sys"}}, "metadata": obj{"is_visually_hidden_from_conversation": true}}},
			"d":    obj{"parent": "hid", "message": msg("assistant", "all", 1.7e9+5, "Roses", obj{"content_type": "image_asset_pointer"})},
		},
	}}
	gptPath := filepath.Join(dir, "conversations.json")
	b, _ = json.Marshal(gpt)
	os.WriteFile(gptPath, b, 0o600)

	c, _ := fakeChat(t, t.TempDir(), 100)
	importInto(t, c, gptPath, importOpts{tools: true}, "y\n")
	importInto(t, c, zipPath, importOpts{}, "y\n")
	want := []string{
		"note: ChatGPT chat \"Code\", from 2023-11-14 22:13:21 UTC (imported)",
		"user: write a poem",
		"tool: python print(1)",
		"echo: 1",
		"ai: Roses\n[image]",
		"note: Claude chat \"Trip\", from 2026-05-01 12:00:00 UTC (imported)",
		"user: Plan a trip\n[file map.pdf]",
		"ai: Sure.",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
}

// A text file's lines become notes, once the user says yes.
func TestImportNotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memories.txt")
	os.WriteFile(path, []byte("I like F#\n\n  I dislike Haskell  \n"), 0o600)
	c, _ := fakeChat(t, t.TempDir(), 100)
	if out := importInto(t, c, path, importOpts{}, "\n"); !strings.HasPrefix(out, "2 notes, one per line.") || len(logged(c)) != 0 {
		t.Fatalf("declined: %q, logged %q", out, logged(c))
	}
	// The chat reads what follows the answer.
	in := strings.NewReader("y\nhello\n")
	if err := importChats(c, path, importOpts{}, in, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := logged(c); !slices.Equal(got, []string{"note: I like F#", "note: I dislike Haskell"}) {
		t.Fatal(got)
	}
	if rest, _ := io.ReadAll(in); string(rest) != "hello\n" {
		t.Fatalf("left for the chat: %q", rest)
	}
}

// The estimate counts one call per line over 512 bytes, up the tree.
func TestSummaryCalls(t *testing.T) {
	for _, tc := range []struct {
		at    int
		sizes []int
		want  int
	}{
		{0, []int{100, 100}, 0},           // both free, and so is their merge
		{0, []int{100, 100, 100, 100}, 0}, // and theirs: 403 bytes
		{0, []int{600}, 1},                // waits for its sibling
		{0, []int{600, 600}, 3},           // two lines, and a merge of 961 bytes
		{1, []int{100}, 1},                // merged with an older line: 581 bytes
		{0, slices.Repeat([]int{1000}, 8), 8 + 4 + 2 + 1},
	} {
		if got := summaryCalls(tc.at, tc.sizes); got != tc.want {
			t.Errorf("summaryCalls(%d, %v) = %d, want %d", tc.at, tc.sizes, got, tc.want)
		}
	}
}
