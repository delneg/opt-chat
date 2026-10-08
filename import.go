package main

import (
	"archive/zip"
	"bufio"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Importing past chats, as UniiChat does: Claude Code and Codex sessions, and
// Claude and ChatGPT exports. Each session goes into the log, oldest first,
// headed by a note naming it, each message at its own date. The user's and
// the assistant's words come in, with their images; tool calls and results
// only when asked for. Thinking never does, as in the live chat.

// session is one past chat.
type session struct {
	app, where string    // "Claude Code", "session in /path"
	start      time.Time // its first message's date: with app and where, it names the session
	msgs       []pastMsg
}

type pastMsg struct {
	kind, text string
	date       time.Time
	images     [][]byte
}

// note heads the session in the log, and names it for the next import.
func (s *session) note() string {
	return fmt.Sprintf("%s %s, from %s (imported)", s.app, s.where, s.start.UTC().Format("2006-01-02 15:04:05 UTC"))
}

// add appends a source message's messages, those that keep takes.
func (s *session) add(keep func(*session, pastMsg) bool, msgs []pastMsg) {
	if s.start.IsZero() && len(msgs) > 0 {
		s.start = msgs[0].date
	}
	for _, m := range msgs {
		if keep(s, m) {
			s.msgs = append(s.msgs, m)
		}
	}
}

var (
	// Claude Code's own lines in the user's turn: commands and their output,
	// interruptions, background-task notices.
	claudeSkip = regexp.MustCompile(`^(<command-|<local-command|<task-notification|\[Request interrupted)`)
	// Codex's injected context: <environment_context>, AGENTS.md, ...
	codexSkip   = regexp.MustCompile(`^(<[a-z_]+[ >]|# AGENTS\.md)`)
	sessionFile = regexp.MustCompile(`(^|/)(projects|sessions|archived_sessions)/`)
)

// readSessions reads the past chats at path, oldest first: a ~/.claude or
// ~/.codex directory (or one under it), one session's .jsonl, or a Claude or
// ChatGPT export (its .zip, or the conversations.json in it). It returns the
// sessions with a message keep takes, and how many it found.
func readSessions(path string, keep func(*session, pastMsg) bool) ([]session, int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	var all []session
	switch ext := strings.ToLower(filepath.Ext(path)); {
	case fi.IsDir():
		abs, _ := filepath.Abs(path)
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") || !sessionFile.MatchString(p) || strings.Contains(p, "/subagents/") {
				return err
			}
			s, err := readSessionFile(p, keep)
			all = append(all, s)
			return err
		})
	case ext == ".jsonl":
		var s session
		s, err = readSessionFile(path, keep)
		all = append(all, s)
	case ext == ".zip" || ext == ".json":
		all, err = readExport(path, keep)
	default:
		err = fmt.Errorf("%s: not a directory of sessions, a session's .jsonl, or an export's .zip or .json", path)
	}
	all = slices.DeleteFunc(all, func(s session) bool { return s.start.IsZero() }) // not a chat
	found := len(all)
	kept := slices.DeleteFunc(all, func(s session) bool { return len(s.msgs) == 0 })
	slices.SortStableFunc(kept, func(a, b session) int { return a.start.Compare(b.start) })
	return kept, found, err
}

// readSessionFile reads one Claude Code or Codex session.
func readSessionFile(path string, keep func(*session, pastMsg) bool) (session, error) {
	f, err := os.Open(path)
	if err != nil {
		return session{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 256<<20)
	s := session{app: "Claude Code", where: "session"}
	var date time.Time
	for n := 0; sc.Scan(); n++ {
		var j struct {
			Type             string `json:"type"`
			Timestamp        string `json:"timestamp"`
			Cwd              string `json:"cwd"`
			IsMeta           bool   `json:"isMeta"`
			IsSidechain      bool   `json:"isSidechain"`
			IsCompactSummary bool   `json:"isCompactSummary"`
			Message          struct {
				Content jsontext.Value `json:"content"`
			} `json:"message"`
			Attachment struct {
				Type   string         `json:"type"`
				Prompt jsontext.Value `json:"prompt"`
			} `json:"attachment"`
			Payload jsontext.Value `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &j) != nil {
			continue
		}
		date = parseDate(j.Timestamp, date)
		if n == 0 && j.Type == "session_meta" {
			s.app, s.where = "Codex", "" // until its meta says whose it is
		}
		if s.app == "Codex" {
			s.add(keep, codexLine(&s, j.Type, j.Payload, date))
			continue
		}
		if s.where == "session" && j.Cwd != "" {
			s.where = "session in " + j.Cwd
		}
		kind := ""
		if j.Type == "user" {
			kind = "user"
		} else if j.Type == "assistant" {
			kind = "opt"
		}
		switch {
		case j.Type == "attachment" && j.Attachment.Type == "queued_command": // sent mid-turn
			var prompt string
			json.Unmarshal(j.Attachment.Prompt, &prompt)
			s.add(keep, pastMsgs("user", prompt, nil, date))
		case kind != "" && !j.IsMeta && !j.IsSidechain && !j.IsCompactSummary:
			s.add(keep, blocksMsgs(kind, contentBlocks(j.Message.Content), date, claudeSkip))
		}
	}
	return s, sc.Err()
}

// codexLine reads one line of a Codex session. A subagent's session is
// skipped whole, as Claude Code's are.
func codexLine(s *session, typ string, payload jsontext.Value, date time.Time) []pastMsg {
	var p struct {
		Type      string         `json:"type"`
		Role      string         `json:"role"`
		Cwd       string         `json:"cwd"`
		Source    jsontext.Value `json:"source"`
		Content   []block        `json:"content"`
		Name      string         `json:"name"`
		Arguments string         `json:"arguments"`
		Input     jsontext.Value `json:"input"`
		Action    struct {
			Command jsontext.Value `json:"command"`
		} `json:"action"`
		Output jsontext.Value `json:"output"`
	}
	json.Unmarshal(payload, &p)
	if typ == "session_meta" {
		var src struct {
			Subagent jsontext.Value `json:"subagent"`
			Internal jsontext.Value `json:"internal"`
		}
		json.Unmarshal(p.Source, &src) // or a plain string, as "cli"
		if len(src.Subagent) == 0 && len(src.Internal) == 0 {
			s.where = "session in " + p.Cwd
		}
		return nil
	}
	if typ != "response_item" || s.where == "" {
		return nil
	}
	switch p.Type {
	case "message":
		switch p.Role {
		case "user":
			return blocksMsgs("user", p.Content, date, codexSkip)
		case "assistant":
			return blocksMsgs("opt", p.Content, date, codexSkip)
		}
	case "function_call":
		return pastMsgs("tool", p.Name+" "+p.Arguments, nil, date)
	case "custom_tool_call":
		in, _ := json.Marshal(map[string]jsontext.Value{"input": p.Input})
		return pastMsgs("tool", p.Name+" "+string(in), nil, date)
	case "local_shell_call":
		in, _ := json.Marshal(map[string]jsontext.Value{"command": p.Action.Command})
		return pastMsgs("tool", "shell "+string(in), nil, date)
	case "function_call_output", "custom_tool_call_output":
		out, imgs := resultContent(p.Output)
		return pastMsgs("echo", out, imgs, date)
	}
	return nil
}

// contentBlocks is a message's content: a string or blocks.
func contentBlocks(v jsontext.Value) []block {
	var s string
	if json.Unmarshal(v, &s) == nil {
		return []block{textBlock(s)}
	}
	var bs []block
	json.Unmarshal(v, &bs)
	return bs
}

// blocksMsgs turns one message's blocks into the log's messages: its words,
// with their images, then each tool call and result.
func blocksMsgs(kind string, bs []block, date time.Time, skip *regexp.Regexp) []pastMsg {
	var words []string
	var imgs [][]byte
	var tools []pastMsg
	for _, b := range bs {
		switch b.Type {
		case "text", "input_text", "output_text":
			if !skip.MatchString(b.Text) {
				words = append(words, b.Text)
			}
		case "image", "input_image":
			if img := blockImage(b); img != nil {
				imgs = append(imgs, img)
			} else {
				words = append(words, "[image]")
			}
		case "tool_use":
			tools = append(tools, pastMsgs("tool", b.Name+" "+string(b.Input), nil, date)...)
		case "tool_result":
			out, timgs := resultContent(b.Content)
			tools = append(tools, pastMsgs("echo", out, timgs, date)...)
		}
	}
	return append(pastMsgs(kind, strings.Join(words, "\n"), imgs, date), tools...)
}

// blockImage decodes an image block: Claude's base64 source, or Codex's data URL.
func blockImage(b block) []byte {
	data, url := "", ""
	json.Unmarshal(b.ImageURL, &url)
	if b.Source != nil && b.Source.Type == "base64" {
		data = b.Source.Data
	} else if _, after, ok := strings.Cut(url, ";base64,"); ok && strings.HasPrefix(url, "data:image/") {
		data = after
	}
	img, err := base64.StdEncoding.DecodeString(data)
	if err != nil || len(img) == 0 {
		return nil
	}
	return img
}

// pastMsgs is one message, or none when it says nothing. A tool's output is
// clipped, as in the live chat; other text is split as Log splits it.
func pastMsgs(kind, text string, imgs [][]byte, date time.Time) []pastMsg {
	if strings.TrimSpace(text) == "" {
		if len(imgs) == 0 {
			return nil
		}
		text = "[image]"
	}
	if kind == "echo" {
		text = clip(text, clipMax)
	}
	return []pastMsg{{kind: kind, text: text, date: date, images: imgs}}
}

func parseDate(s string, last time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return last
}

func unixDate(sec float64, last time.Time) time.Time {
	if sec <= 0 {
		return last
	}
	return time.Unix(0, int64(sec*1e9))
}

// readExport reads a Claude or ChatGPT data export.
func readExport(path string, keep func(*session, pastMsg) bool) ([]session, error) {
	var data []byte
	var err error
	if strings.EqualFold(filepath.Ext(path), ".zip") {
		data, err = zipFile(path, "conversations.json")
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	type file struct {
		Name string `json:"file_name"`
	}
	var chats []struct {
		// Claude
		Name     string `json:"name"`
		Created  string `json:"created_at"`
		Messages []struct {
			Sender  string         `json:"sender"`
			Text    string         `json:"text"`
			Content jsontext.Value `json:"content"`
			Created string         `json:"created_at"`
			Files   []file         `json:"files"`
			Attach  []file         `json:"attachments"`
		} `json:"chat_messages"`
		// ChatGPT
		Title      string                 `json:"title"`
		CreateTime float64                `json:"create_time"`
		Current    string                 `json:"current_node"`
		Mapping    map[string]chatGPTNode `json:"mapping"`
	}
	if err := json.Unmarshal(data, &chats); err != nil {
		return nil, fmt.Errorf("%s: not a Claude or ChatGPT export: %v", path, err)
	}
	var out []session
	for _, c := range chats {
		if c.Mapping != nil {
			s := session{app: "ChatGPT", where: fmt.Sprintf("chat %q", c.Title)}
			s.add(keep, chatGPTMsgs(c.Mapping, c.Current, unixDate(c.CreateTime, time.Time{})))
			out = append(out, s)
			continue
		}
		s := session{app: "Claude", where: fmt.Sprintf("chat %q", c.Name)}
		date := parseDate(c.Created, time.Time{})
		for _, m := range c.Messages {
			date = parseDate(m.Created, date)
			bs := []block{textBlock(m.Text)}
			if len(m.Content) > 0 && string(m.Content) != "null" {
				bs = contentBlocks(m.Content)
			}
			for _, f := range slices.Concat(m.Attach, m.Files) {
				bs = append(bs, textBlock("[file "+f.Name+"]"))
			}
			kind := "opt"
			if m.Sender == "human" {
				kind = "user"
			}
			s.add(keep, blocksMsgs(kind, bs, date, claudeSkip))
		}
		out = append(out, s)
	}
	return out, nil
}

type chatGPTNode struct {
	Parent  string `json:"parent"`
	Message *struct {
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		Recipient  string  `json:"recipient"`
		CreateTime float64 `json:"create_time"`
		Content    struct {
			Type  string           `json:"content_type"`
			Parts []jsontext.Value `json:"parts"`
			Text  string           `json:"text"`
		} `json:"content"`
		Metadata struct {
			Hidden bool `json:"is_visually_hidden_from_conversation"`
		} `json:"metadata"`
	} `json:"message"`
}

// chatGPTMsgs walks a ChatGPT chat from its current node back to its root:
// the branch the user last saw.
func chatGPTMsgs(mapping map[string]chatGPTNode, current string, date time.Time) []pastMsg {
	var path []chatGPTNode
	for id, seen := current, map[string]bool{}; id != "" && !seen[id]; id = mapping[id].Parent {
		seen[id] = true
		path = append(path, mapping[id])
	}
	slices.Reverse(path)
	var out []pastMsg
	for _, n := range path {
		m := n.Message
		if m == nil || m.Metadata.Hidden || m.Content.Type == "thoughts" || m.Content.Type == "reasoning_recap" {
			continue
		}
		date = unixDate(m.CreateTime, date)
		var parts []string
		for _, p := range m.Content.Parts {
			var s string
			var o struct {
				Text string `json:"text"`
				Type string `json:"content_type"`
			}
			if json.Unmarshal(p, &s) == nil {
				parts = append(parts, s)
			} else if json.Unmarshal(p, &o) == nil && o.Text != "" {
				parts = append(parts, o.Text)
			} else if strings.Contains(o.Type, "image") {
				parts = append(parts, "[image]")
			}
		}
		if len(m.Content.Parts) == 0 {
			parts = []string{m.Content.Text}
		}
		text := strings.Join(parts, "\n")
		switch {
		case m.Author.Role == "user":
			out = append(out, pastMsgs("user", text, nil, date)...)
		case m.Author.Role == "assistant" && m.Recipient == "all":
			out = append(out, pastMsgs("opt", text, nil, date)...)
		case m.Author.Role == "assistant":
			out = append(out, pastMsgs("tool", m.Recipient+" "+text, nil, date)...)
		case m.Author.Role == "tool":
			out = append(out, pastMsgs("echo", text, nil, date)...)
		}
	}
	return out
}

func zipFile(path, name string) ([]byte, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == name || strings.HasSuffix(f.Name, "/"+name) {
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer r.Close()
			return io.ReadAll(r)
		}
	}
	return nil, fmt.Errorf("%s: no %s in it", path, name)
}

// imported gives, for each session already in the log, the date of its last
// message there. A session's messages follow its note and are older than it:
// the note is dated at the import, they at their own dates.
func (c *Chat) imported() map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	done := map[string]time.Time{}
	cur, at := "", time.Time{}
	for _, m := range c.msgs {
		switch {
		case m.Kind == "note" && strings.HasSuffix(m.Text, " (imported)"):
			cur, at = m.Text, m.Date
		case cur != "" && m.Date.Before(at):
			if m.Date.After(done[cur]) {
				done[cur] = m.Date
			}
		default:
			cur = ""
		}
	}
	return done
}

// keeper takes the messages to import: from since on, past what an earlier
// import logged, and tool calls and results only with tools.
func keeper(done map[string]time.Time, since time.Time, tools bool) func(*session, pastMsg) bool {
	return func(s *session, m pastMsg) bool {
		last, seen := done[s.note()]
		return (tools || m.kind != "tool" && m.kind != "echo") && !m.date.Before(since) && (!seen || m.date.After(last))
	}
}

// logSessions logs each session, its note first, in one write per session.
func (c *Chat) logSessions(sessions []session) {
	for _, s := range sessions {
		ms := []Msg{{Kind: "note", Text: s.note(), Date: time.Now()}}
		for _, m := range s.msgs {
			var names []string
			for _, img := range m.images {
				if name, err := c.saveImage(img); err == nil {
					names = append(names, name)
				} else {
					m.text += "\n[image not kept: " + err.Error() + "]"
				}
			}
			ms = append(ms, Msg{Kind: m.kind, Text: m.text, Date: m.date, Images: names})
		}
		c.logMsgs(ms)
	}
}

// summaryCalls estimates the model calls that summarize messages of these
// sizes logged from id at on: one for each line whose source is over 512
// bytes (or has images: give it a size over 512), a built line being ~480.
func summaryCalls(at int, sizes []int) int {
	calls := 0
	line := func(s int) int {
		if s > nodeMax {
			calls++
			return 480
		}
		return s
	}
	for nodes := sizes; len(nodes) > 0; at /= 2 {
		var up []int
		j := 0
		if at%2 == 1 { // its left half is an older line
			up, j = append(up, 480+1+line(nodes[0])), 1
		}
		for ; j+1 < len(nodes); j += 2 {
			up = append(up, line(nodes[j])+1+line(nodes[j+1]))
		}
		if j < len(nodes) {
			line(nodes[j]) // its right half has yet to come
		}
		nodes = up
	}
	return calls
}

// importPlan says what an import adds, and what summarizing it takes.
func importPlan(c *Chat, sessions []session) string {
	c.mu.Lock()
	at := len(c.msgs)
	c.mu.Unlock()
	var sizes []int
	n, bytes := 0, 0
	var first, last time.Time
	for _, s := range sessions {
		sizes = append(sizes, len("note: "+s.note()))
		for _, m := range s.msgs {
			from := len(sizes)
			for _, part := range splitText(m.text, clipMax) {
				sizes = append(sizes, len(m.kind)+2+len(part))
			}
			if len(m.images) > 0 {
				sizes[from] = nodeMax + 1
			}
			n, bytes = n+1, bytes+len(m.text)
			if first.IsZero() || m.date.Before(first) {
				first = m.date
			}
			if m.date.After(last) {
				last = m.date
			}
		}
	}
	return fmt.Sprintf("%s (%.1f MB) from %s, %s to %s. Summarizing them takes about\n"+
		"%s to the compaction model, and turns wait until they are done.",
		count(n, "message"), float64(bytes)/1e6, count(len(sessions), "session"), first.Local().Format(time.DateOnly),
		last.Local().Format(time.DateOnly), count(summaryCalls(at, sizes), "call"))
}

// count is n with thousands separated, and the noun after it.
func count(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return countN(n) + " " + noun
}

func countN(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

var errDeclined = errors.New("nothing imported")

// importChats imports the chats at path, or a text file's lines as notes,
// after saying what that adds and asking: the log is append-only.
func importChats(c *Chat, path string, since time.Time, tools bool, in io.Reader, out io.Writer) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() && !slices.Contains([]string{".jsonl", ".json", ".zip"}, strings.ToLower(filepath.Ext(path))) {
		b, err := os.ReadFile(path) // memories from before the chat
		if err != nil {
			return err
		}
		var notes []Msg
		for line := range strings.Lines(string(b)) {
			if line = strings.TrimSpace(line); line != "" {
				notes = append(notes, Msg{Kind: "note", Text: line, Date: time.Now()})
			}
		}
		fmt.Fprintf(out, "%s, one per line.\n", count(len(notes), "note"))
		if len(notes) == 0 {
			return nil
		}
		if !confirm(in, out) {
			return errDeclined
		}
		c.logMsgs(notes)
		fmt.Fprintln(out, "Imported: the next run summarizes them.")
		return nil
	}
	sessions, found, err := readSessions(path, keeper(c.imported(), since, tools))
	if err != nil {
		return err
	}
	if found == 0 {
		return fmt.Errorf("no chats in %s", path)
	}
	if len(sessions) == 0 {
		fmt.Fprintf(out, "Nothing new in the %s found: all in the chat already, or older than -import-since.\n", count(found, "session"))
		return nil
	}
	fmt.Fprintln(out, importPlan(c, sessions))
	if !tools {
		fmt.Fprintln(out, "Without tool calls and results: -import-tools adds them.")
	}
	if !confirm(in, out) {
		return errDeclined
	}
	c.logSessions(sessions)
	fmt.Fprintln(out, "Imported: the next run summarizes them.")
	return nil
}

func confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Import? The log is append-only: this can't be undone. [y/N] ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	return slices.Contains([]string{"y", "yes"}, strings.ToLower(strings.TrimSpace(answer)))
}
