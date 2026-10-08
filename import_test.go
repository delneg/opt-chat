package main

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"slices"
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
		claudeLine("user", "2026-09-01T10:00:07Z", []obj{
			{"type": "text", "text": "See [Image #1]"},
			{"type": "image", "source": obj{"type": "base64", "media_type": "image/png", "data": img}},
		}),
		obj{"type": "attachment", "timestamp": "2026-09-01T10:00:08Z", "attachment": obj{"type": "queued_command", "prompt": "and the tests"}},
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
	if out := importInto(t, c, filepath.Join(home, ".claude"), importOpts{}, "n\n"); !strings.Contains(out, "5 messages (0.0 MB) from 2 sessions, ") ||
		!strings.Contains(out, "-import-tools adds them") || len(logged(c)) != 0 {
		t.Fatalf("declined: %q, logged %q", out, logged(c))
	}
	importInto(t, c, filepath.Join(home, ".claude"), importOpts{skip: []string{"", "/bots"}}, "y\n")
	want := []string{
		"note: Claude Code session in /work, from 2026-09-01 10:00:00 UTC (imported)",
		"user: Fix the build",
		"opt: Looking.",
		"user: See [Image #1]",
		"user: and the tests",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
	}
	if m := c.msgs[3]; len(m.Images) != 1 || !m.Date.Equal(time.Date(2026, 9, 1, 10, 0, 7, 0, time.UTC)) {
		t.Fatalf("the image message: %+v", m)
	}
	if out := importInto(t, c, filepath.Join(home, ".claude"), importOpts{skip: []string{"/bots"}}, "y\n"); !strings.HasPrefix(out, "Nothing new in the 2 sessions found") {
		t.Fatalf("a second import: %q", out)
	}

	// The session goes on: the next import adds the rest, here with tools.
	c.Log("user", "a live message") // after the import: never counted as the session's
	jsonl(t, file,
		claudeLine("assistant", "2026-09-02T09:00:00Z", []obj{{"type": "text", "text": "Done."}, {"type": "tool_use", "name": "Read", "input": obj{"file_path": "a"}}}),
		claudeLine("user", "2026-09-02T09:00:01Z", []obj{{"type": "tool_result", "content": "contents of " + strings.Repeat("y", 40_000)}}),
	)
	importInto(t, c, file, importOpts{tools: true}, "y\n")
	got := logged(c)[6:]
	if len(got) != 4 || got[0] != want[0] || got[1] != "opt: Done." || got[2] != `tool: Read {"file_path":"a"}` ||
		!strings.HasPrefix(got[3], "echo: contents of ") || !strings.Contains(got[3], " characters cut ...]") {
		t.Fatalf("the rest: %q", got)
	}
	if out := importInto(t, c, file, importOpts{since: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC), tools: true}, "y\n"); !strings.HasPrefix(out, "Nothing new") {
		t.Fatalf("-import-since past the end: %q", out)
	}
}

// Codex sessions: injected context and subagents' sessions stay out.
func TestImportCodex(t *testing.T) {
	home := t.TempDir()
	item := func(at string, p obj) obj { return obj{"type": "response_item", "timestamp": at, "payload": p} }
	jsonl(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "01", "rollout-1.jsonl"),
		obj{"type": "session_meta", "timestamp": "2026-09-01T08:00:00Z", "payload": obj{"cwd": "/code", "source": "cli"}},
		item("2026-09-01T08:00:01Z", obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "<environment_context>x</environment_context>"}}}),
		item("2026-09-01T08:00:02Z", obj{"type": "message", "role": "user", "content": []obj{{"type": "input_text", "text": "list files"}}}),
		item("2026-09-01T08:00:03Z", obj{"type": "function_call", "name": "shell", "arguments": `{"command":["ls"]}`}),
		item("2026-09-01T08:00:04Z", obj{"type": "function_call_output", "output": "a b"}),
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
		"note: Codex session in /code, from 2026-09-01 08:00:02 UTC (imported)",
		"user: list files",
		`tool: shell {"command":["ls"]}`,
		"echo: a b",
		"opt: Two files.",
	}
	if got := logged(c); !slices.Equal(got, want) {
		t.Fatalf("logged %q\nwant %q", got, want)
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
		"opt: Roses\n[image]",
		"note: Claude chat \"Trip\", from 2026-05-01 12:00:00 UTC (imported)",
		"user: Plan a trip\n[file map.pdf]",
		"opt: Sure.",
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
	importInto(t, c, path, importOpts{}, "y\n")
	if got := logged(c); !slices.Equal(got, []string{"note: I like F#", "note: I dislike Haskell"}) {
		t.Fatal(got)
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
