package main

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samber/lo"
)

// What Opt sees, for the page: the view as a list of lines, each opened
// into its halves on a click, after UniiChat's blocks (View.ts part_rows,
// web/src/Html.ts "Sum" and "Box", at
// https://github.com/VictorTaelin/UniiChat); and /tree, the whole memory
// as a page of its own, after its Memory.page and Server.ts "/tree".

// memLine is one line of the view or of a zoom, as the page shows it.
type memLine struct {
	ID    int       `json:"id"`
	N     int       `json:"n"`
	Text  string    `json:"text"`  // as a call sees it; a message not summarized yet: its head
	Built bool      `json:"built"` // false: not summarized yet
	Bytes int       `json:"bytes"` // its share of a call's view: the whole line, name and newline too
	From  time.Time `json:"from"`  // its first message's date
	To    time.Time `json:"to"`    // its last's
}

type (
	memViewReply struct {
		T     string    `json:"t"` // memview
		Lines []memLine `json:"lines"`
	}
	zoomReply struct {
		T     string    `json:"t"` // zoom
		ID    int       `json:"id"`
		N     int       `json:"n"`
		Lines []memLine `json:"lines,omitzero"` // n > 1: its two halves
		Msg   *Msg      `json:"msg,omitzero"`   // n = 1: the message whole
		Error string    `json:"error,omitzero"`
	}
)

// memLine is line k as callLine has it, its name apart. Caller holds c.mu.
func (c *Chat) memLine(k key) memLine {
	line := c.callLine(k)
	_, text, _ := strings.Cut(line, "|")
	text = strings.TrimSuffix(strings.TrimSuffix(text, unbuilt), " ")
	return memLine{ID: k.start(), N: 1 << k.L, Text: text, Built: c.built(k), Bytes: len(line) + 1,
		From: c.msgs[k.start()].Date, To: c.msgs[min(k.end(), len(c.msgs))-1].Date}
}

// memView is the view as a call would see it now (callView), line by line.
func (c *Chat) memView() []memLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.viewLines()
}

// viewLines is memView's. Caller holds c.mu.
func (c *Chat) viewLines() []memLine {
	return lo.Map(c.callKeys(), func(k key, _ int) memLine { return c.memLine(k) })
}

// memZoom opens line id+n as Opt's zoom does: its two halves, or for n = 1
// the message whole.
func (c *Chat) memZoom(id, n int) zoomReply {
	c.mu.Lock()
	defer c.mu.Unlock()
	reply := zoomReply{T: "zoom", ID: id, N: n}
	k, ok := c.keyOf(id, n)
	switch {
	case !ok:
		reply.Error = c.noLine(id, n)
	case n == 1:
		m := c.msgs[id]
		reply.Msg = &m
	default:
		a, b := k.kids()
		reply.Lines = []memLine{c.memLine(a), c.memLine(b)}
	}
	return reply
}

// The /tree page: one section at a time, as UniiChat's shows one (its
// Memory.page): the view, the messages, or a level of the tree, a page of
// it at a time, so a chat of tens of thousands of messages opens at once.
const (
	treePage    = 500 // lines a page of a level shows
	treeMsgPage = 200 // and of the messages, which are longer
)

type treeItem struct {
	Head  string
	Text  string
	Built bool
}

type treeLink struct {
	Name, Href string
	On         bool
}

type treeData struct {
	Title    string
	Sections []treeLink
	Note     string
	Items    []treeItem
	Pages    []treeLink // first, previous, next, last
	PageOf   string
}

var treeTmpl = template.Must(template.New("tree").Parse(`{{define "pages"}}{{if .Pages}}<nav class="tree-pages">{{range .Pages}}{{if .Href}}<a href="{{.Href}}">{{.Name}}</a>{{else}}<span>{{.Name}}</span>{{end}} {{end}}<span class="dim">{{.PageOf}}</span></nav>{{end}}{{end}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>{{.Title}} · opt-chat memory</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E%3Ctext x='1' y='13' font-size='14' fill='%23d97757'%3E%E2%9C%BB%3C/text%3E%3C/svg%3E">
<link rel="stylesheet" href="/static/style.css">
</head><body class="tree">
<nav class="tree-nav"><a href="/">← the chat</a>{{range .Sections}} <a href="{{.Href}}"{{if .On}} class="on" aria-current="page"{{end}}>{{.Name}}</a>{{end}}</nav>
<p class="tree-note">{{.Note}}</p>
{{template "pages" .}}
<main>{{range .Items}}<section class="tree-item{{if not .Built}} unbuilt{{end}}"><h4>{{.Head}}{{if not .Built}} · not summarized yet{{end}}</h4><pre>{{.Text}}</pre></section>
{{end}}</main>
{{template "pages" .}}
</body></html>
`))

// tree serves /tree: ?s=view (the default), msgs, or a level's number; ?p=
// the page, from 0.
func (w *webServer) tree(rw http.ResponseWriter, r *http.Request) {
	d, ok := w.c.treeData(r.URL.Query().Get("s"), r.URL.Query().Get("p"))
	if !ok {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-cache")
	treeTmpl.Execute(rw, d)
}

func treeWhen(a, b time.Time) string {
	const f = "2006-01-02 15:04:05"
	if a.Equal(b) {
		return a.Local().Format(f)
	}
	return a.Local().Format(f) + " – " + b.Local().Format(f)
}

// treeData is a section of /tree; false: no such section or page.
func (c *Chat) treeData(s, p string) (treeData, bool) {
	if s == "" {
		s = "view"
	}
	if p == "" {
		p = "0"
	}
	page, err := strconv.Atoi(p)
	if err != nil || page < 0 {
		return treeData{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.msgs)
	top := 0 // the highest level with a whole node
	for n>>(top+1) > 0 {
		top++
	}
	var d treeData
	d.Sections = append(d.Sections, treeLink{"view", "/tree", s == "view"}, treeLink{"messages", "/tree?s=msgs", s == "msgs"})
	for l := 0; l <= top && n > 0; l++ {
		d.Sections = append(d.Sections, treeLink{"L" + strconv.Itoa(l), "/tree?s=" + strconv.Itoa(l), s == strconv.Itoa(l)})
	}
	item := func(k key) treeItem {
		t, built := c.tree[k]
		head := k.name() + " · " + treeWhen(c.msgs[k.start()].Date, c.msgs[min(k.end(), n)-1].Date)
		if built {
			head += fmt.Sprintf(" · %d B", len(t))
		}
		return treeItem{Head: head, Text: t, Built: built}
	}
	// paged is this page's share of count items, per a page, and links
	// to the others
	paged := func(count, per int, href string) (int, int, bool) {
		pages := max(1, (count+per-1)/per)
		if page >= pages {
			return 0, 0, false
		}
		link := func(name string, to int) treeLink {
			if to < 0 || to >= pages || to == page {
				return treeLink{Name: name}
			}
			return treeLink{Name: name, Href: href + "&p=" + strconv.Itoa(to)}
		}
		if pages > 1 {
			d.Pages = []treeLink{link("« first", 0), link("‹ previous", page-1), link("next ›", page+1), link("last »", pages-1)}
			d.PageOf = fmt.Sprintf("page %d of %d", page+1, pages)
		}
		return page * per, min(count, (page+1)*per), true
	}
	switch s {
	case "view":
		if page > 0 {
			return treeData{}, false
		}
		d.Title = "the view"
		lines := c.viewLines()
		d.Note = fmt.Sprintf("What each turn reads instead of the whole chat: %d lines, oldest first, as a call sees it now. Opt opens any line into its two halves with zoom.", len(lines))
		for _, l := range lines {
			d.Items = append(d.Items, treeItem{Head: fmt.Sprintf("%d+%d · %s · %d B", l.ID, l.N, treeWhen(l.From, l.To), len(l.Text)), Text: l.Text, Built: l.Built})
		}
	case "msgs":
		d.Title = "the messages"
		d.Note = fmt.Sprintf("The log: all %d messages, oldest first, as they were said.", n)
		from, to, ok := paged(n, treeMsgPage, "/tree?s=msgs")
		if !ok {
			return treeData{}, false
		}
		for _, m := range c.msgs[from:to] {
			d.Items = append(d.Items, treeItem{Head: fmt.Sprintf("#%d · %s · %s · %d B", m.I, treeWhen(m.Date, m.Date), m.Kind, m.Size), Text: m.Text, Built: true})
		}
	default:
		l, err := strconv.Atoi(s)
		if err != nil || l < 0 || l > top {
			return treeData{}, false
		}
		count := n >> l
		built := 0
		for i := range count {
			if c.built(key{l, i}) {
				built++
			}
		}
		d.Title = "level " + s
		d.Note = fmt.Sprintf("Level %d: each line summarizes %d messages; %d of %d summarized.", l, 1<<l, built, count)
		from, to, ok := paged(count, treePage, "/tree?s="+s)
		if !ok {
			return treeData{}, false
		}
		for i := from; i < to; i++ {
			d.Items = append(d.Items, item(key{l, i}))
		}
	}
	return d, true
}
