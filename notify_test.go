package main

import (
	"encoding/json/v2"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// events are the notifications the pages were asked for, in order.
func events(r *recorder) []eventUpdate {
	var out []eventUpdate
	for _, u := range r.all() {
		if e, ok := u.(eventUpdate); ok {
			out = append(out, e)
		}
	}
	return out
}

// quietSounder takes sounds into a channel and plays none.
func quietSounder() (*sounder, chan string) {
	s := &sounder{in: make(chan string, 64)}
	return s, s.in
}

func drain(ch chan string) []string {
	var out []string
	for {
		select {
		case c := <-ch:
			out = append(out, c)
		default:
			return out
		}
	}
}

// Each kind of event plays its sound and asks the pages for a
// notification as its switches say, by default or as the user set them;
// a subagent's own are of the sub kinds; a muted subagent, and its own,
// are told of by neither.
func TestNotifyRules(t *testing.T) {
	t.Parallel()
	h := newHub()
	r := &recorder{}
	h.print = r.add
	s, ch := quietSounder()
	h.sounds = s

	h.event("done", "", "All\n  done.")
	h.event("took", "", "")
	if got := drain(ch); !slices.Equal(got, []string{soundDone, soundAck}) {
		t.Fatalf("sounds %v", got)
	}
	ev := events(r)
	if len(ev) != 1 || ev[0].Kind != "opt.done" || ev[0].Title != "Opt ended its turn" || ev[0].Body != "All done." || !ev[0].Sound || ev[0].ID == "" {
		t.Fatalf("events %+v", ev)
	}

	h.setParent("Help", "Lead")
	h.event("report", "Help", "found it")    // sub.report: a sound, no notification by default
	h.event("error", "Help", "boom")         // sub.error: both
	h.event("report", "Lead", "all of it")   // agent.report: both
	h.event("app.start", "", "")             // a sound alone
	h.event("app.error", "", "reload: nope") // both
	if got := drain(ch); !slices.Equal(got, []string{soundDone, soundError, soundDone, soundStart, soundError}) {
		t.Fatalf("sounds %v", got)
	}
	ev = events(r)[1:]
	var got []string
	for _, e := range ev {
		got = append(got, e.Kind+": "+e.Title+": "+e.Body)
	}
	want := []string{"sub.error: [Help] failed: boom", "agent.report: [Lead] reported to Opt: all of it", "app.error: opt-chat failed to reload, or to write a memory: reload: nope"}
	if !slices.Equal(got, want) {
		t.Fatalf("events %q", got)
	}
	ids := map[string]bool{}
	for _, e := range events(r) {
		ids[e.ID] = true
	}
	if len(ids) != 4 {
		t.Fatalf("ids %v", ids)
	}

	h.setNotify(notifySettings{Muted: []string{"Lead"}, Focused: true, Rules: map[string]notifyRule{"opt.done": {}, "opt.took": {Desk: true}}})
	n := len(events(r))
	h.event("report", "Lead", "x")
	h.event("error", "Help", "x") // its parent muted
	h.event("done", "", "x")      // both off
	if got := drain(ch); len(got) != 0 || len(events(r)) != n {
		t.Fatalf("muted or off: sounds %v, events %+v", got, events(r)[n:])
	}
	h.event("took", "", "hi")
	h.event("error", "Other", "x")
	if got := drain(ch); !slices.Equal(got, []string{soundError}) {
		t.Fatalf("sounds %v", got)
	}
	ev = events(r)[n:]
	if len(ev) != 2 || ev[0].Kind != "opt.took" || ev[0].Sound || !ev[0].Focused || ev[1].Title != "[Other] failed" {
		t.Fatalf("events %+v", ev)
	}

	h.event("nonesuch", "", "") // a bug: warned, nothing told
	if got := drain(ch); len(got) != 0 || len(events(r)) != n+2 {
		t.Fatal("an unknown kind was told of")
	}
	var nilHub *hub
	nilHub.event("done", "", "") // no hub: nothing
}

// A question waiting for the user's yes is told of as its asker's.
func TestNotifyAsks(t *testing.T) {
	t.Parallel()
	h := newHub()
	r := &recorder{}
	h.print = r.add
	h.setParent("Help", "Lead")
	h.setAsks([]ask{{ID: 1, Tool: "Bash", Desc: "lists files"}})
	h.setAsks([]ask{{ID: 1, Tool: "Bash", Desc: "lists files"}, {ID: 2, Who: "Lead", Tool: "Write"}, {ID: 3, Who: "Help", Tool: "Edit"}})
	h.setAsks(nil)
	var got []string
	for _, e := range events(r) {
		got = append(got, e.Kind+" "+e.Who+": "+e.Body)
	}
	if want := []string{"opt.ask : Bash: lists files", "agent.ask Lead: Write", "sub.ask Help: Edit"}; !slices.Equal(got, want) {
		t.Fatalf("events %q", got)
	}
}

// A subagent's start and report, and its own subagent's start, are told of
// by their kinds.
func TestAgentNotify(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	r := record(c)
	c.hub.setNotify(notifySettings{Rules: map[string]notifyRule{"agent.start": {Desk: true}, "sub.start": {Desk: true}}})
	a, _ := fakeAgent(t, c)
	a.agentMCP = `{"url":"x?agent"}`
	a.report = func(string) {}
	kinds := func() []string {
		var out []string
		for _, e := range events(r) {
			out = append(out, e.Kind+" "+e.Who)
		}
		return out
	}
	a.Spawn("Scout", "look around", "", "", 0, nil)
	waitFor(t, "its report", func() bool { return slices.Contains(kinds(), "agent.report Scout") })
	if !slices.Contains(kinds(), "agent.start Scout") {
		t.Fatalf("events %q", kinds())
	}
	callTool(a, false, "", "spawn", toolArgs{Name: "Lead", Task: "wait-for-message", Spawns: true})
	waitFor(t, "its record", func() bool { rec, _ := a.loadRecord("Lead"); return rec.Session != "" })
	if out, _ := callTool(a, true, "Lead", "spawn", toolArgs{Name: "Help", Task: "wait-for-message"}); !strings.HasPrefix(out, "Started [Help]") {
		t.Fatal(out)
	}
	waitFor(t, "its start", func() bool { return slices.Contains(kinds(), "sub.start Help") })
	for _, e := range events(r) {
		if e.Kind == "agent.report" && e.Body != "" && !strings.HasPrefix(e.Title, "[Scout]") {
			t.Fatalf("event %+v", e)
		}
	}
}

// The page's panel lists every kind with its switches, and flips them,
// a subagent's mute and whether a page in front shows notifications; each
// change is kept in settings.json and holds at once.
func TestWebNotify(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	ws, err := wt.dial(0, http.Header{"Cookie": {wt.cookie}})
	if err != nil {
		t.Fatal(err)
	}
	ws.next("hello", typ("hello"))
	row := func(u map[string]any, key string) map[string]any {
		for _, r := range u["notify"].([]any) {
			if m := r.(map[string]any); m["key"] == key {
				return m
			}
		}
		t.Fatalf("no row %s in %v", key, u)
		return nil
	}
	ws.send(map[string]any{"t": "notify"})
	u := ws.next("the list", typ("notify"))
	if n := len(u["notify"].([]any)); n != len(notifyKinds) {
		t.Fatalf("%d rows", n)
	}
	if r := row(u, "opt.done"); r["sound"] != true || r["desk"] != true || r["who"] != "Opt" || r["label"] != "ended its turn" {
		t.Fatalf("row %v", r)
	}
	ws.send(map[string]any{"t": "notify", "action": "desk", "name": "opt.took"})
	u = ws.next("desk", typ("notify"))
	if r := row(u, "opt.took"); r["desk"] != true || r["sound"] != true {
		t.Fatalf("row %v", r)
	}
	ws.send(map[string]any{"t": "notify", "action": "sound", "name": "opt.took"})
	ws.next("sound", typ("notify"))
	ws.send(map[string]any{"t": "notify", "action": "mute", "name": "Scout"})
	ws.next("mute", typ("notify"))
	ws.send(map[string]any{"t": "notify", "action": "focused"})
	u = ws.next("focused", typ("notify"))
	if u["focused"] != true || len(u["muted"].([]any)) != 1 {
		t.Fatalf("reply %v", u)
	}
	s := loadSettings(wt.c.dir)
	if r := s.Notify.Rules["opt.took"]; !r.Desk || r.Sound || !s.Notify.Focused || !slices.Equal(s.Notify.Muted, []string{"Scout"}) {
		t.Fatalf("settings %+v", s.Notify)
	}
	wt.c.hub.mu.Lock()
	held := wt.c.hub.notify
	wt.c.hub.mu.Unlock()
	if !held.Focused || !held.Rules["opt.took"].Desk {
		t.Fatalf("hub's %+v", held)
	}
	ws.send(map[string]any{"t": "notify", "action": "mute", "name": "Scout"}) // and back
	if u = ws.next("unmute", typ("notify")); len(u["muted"].([]any)) != 0 {
		t.Fatalf("reply %v", u)
	}
	for _, bad := range []map[string]any{{"action": "desk", "name": "opt.nonesuch"}, {"action": "explode"}, {"action": "mute"}} {
		bad["t"] = "notify"
		ws.send(bad)
		if u = ws.next("an error", typ("notify")); u["error"] == nil {
			t.Fatalf("%v: %v", bad, u)
		}
	}
	if s2 := loadSettings(wt.c.dir); len(s2.Notify.Rules) != 1 || len(s2.Notify.Muted) != 0 {
		t.Fatalf("settings after errors %+v", s2.Notify)
	}
}

// The page can be installed as an app: its manifest, to holders of the key
// alone since it carries it, starts the app at the address that sets the
// cookie, as an app's own cookie jar may need; its icons need no key.
func TestWebManifest(t *testing.T) {
	t.Parallel()
	wt := startWeb(t)
	resp := wt.do("GET", "/manifest.webmanifest", nil, map[string]string{"Cookie": wt.cookie})
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/manifest+json") {
		t.Fatalf("manifest: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var m struct {
		Name     string `json:"name"`
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
		} `json:"icons"`
	}
	b, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "opt-chat" || m.StartURL != "/?t="+wt.w.token || m.Scope != "/" || m.Display != "standalone" || len(m.Icons) < 2 {
		t.Fatalf("manifest %s", b)
	}
	for _, ic := range m.Icons {
		resp := wt.do("GET", ic.Src, nil, nil) // no cookie
		if resp.StatusCode != 200 {
			t.Fatalf("icon %s: %d", ic.Src, resp.StatusCode)
		}
	}
	if resp := wt.do("GET", "/manifest.webmanifest", nil, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("manifest without the key: %d", resp.StatusCode)
	}
	for _, p := range []string{"/static/app.js", "/static/icon-192.png.map", "/static/icon"} {
		if resp := wt.do("GET", p, nil, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without the key: %d", p, resp.StatusCode)
		}
	}
	if resp := wt.do("GET", "/static/icon-192.png", nil, map[string]string{"Host": "evil.example:80"}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("icon to another host: %d", resp.StatusCode)
	}
	resp = wt.do("GET", "/", nil, map[string]string{"Cookie": wt.cookie})
	page, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(page), `<link rel="manifest" href="/manifest.webmanifest" crossorigin="use-credentials">`) {
		t.Fatal("the page links no manifest")
	}
	if _, err := os.Stat(filepath.Join("web", "icon-512.png")); err != nil {
		t.Fatal(err)
	}
}
