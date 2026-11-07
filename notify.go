package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Notifications: what happens, told by a sound (sound.go) and by a desktop
// notification the page shows through the browser's Notification API. Each
// kind of event, of Opt, of a subagent or of a subagent's own, has a switch
// for each, kept in <dir>/settings.json; a subagent can be muted whole,
// its own with it. opt-chat plays the sound; the pages show the
// notification, so a page must be open, in a tab or installed as an app.

// notifyKind is one kind of event.
type notifyKind struct {
	Key   string // "who.kind": opt, agent (Opt's subagents and the user's own), sub (a subagent's own), app
	Label string // what happened, as the page lists it
	Sound string // its CESP category
	Desk  bool   // a desktop notification, by default
	Quiet bool   // no sound, by default
}

// notifyKinds are the kinds, in the order the page lists them. Sounds are
// on by default where opt-chat played them before notifications came;
// notifications, where the user is waited on or told what they asked for.
var notifyKinds = []notifyKind{
	{"opt.took", "took your message", soundAck, false, false},
	{"opt.done", "ended its turn", soundDone, true, false},
	{"opt.ask", "asks to run a tool", soundAsk, true, false},
	{"opt.error", "failed a turn", soundError, true, false},
	{"opt.wake", "woke up at a time it set", soundAck, false, true},
	{"agent.start", "started or resumed", soundStart, false, false},
	{"agent.took", "took your message, or went back to work", soundAck, false, false},
	{"agent.report", "reported to Opt", soundDone, true, false},
	{"agent.answer", "answered you", soundDone, true, false},
	{"agent.ask", "asks to run a tool", soundAsk, true, false},
	{"agent.compact", "compacted its context mid-task", soundLimit, false, false},
	{"agent.error", "failed", soundError, true, false},
	{"sub.start", "started or resumed", soundStart, false, false},
	{"sub.took", "took your message, or went back to work", soundAck, false, false},
	{"sub.report", "reported to the subagent that started it", soundDone, false, false},
	{"sub.answer", "answered you", soundDone, true, false},
	{"sub.ask", "asks to run a tool", soundAsk, true, false},
	{"sub.compact", "compacted its context mid-task", soundLimit, false, false},
	{"sub.error", "failed", soundError, true, false},
	{"app.start", "started or reloaded", soundStart, false, false},
	{"app.reload", "began a reload", soundAck, false, false},
	{"app.reset", "resumed work after a usage limit's reset", soundStart, true, false},
	{"app.error", "failed to reload, or to write a memory", soundError, true, false},
}

// notifyWho names the senders of each kind's first part, as the page lists them.
var notifyWho = map[string]string{"opt": "Opt", "agent": "Subagents", "sub": "Their subagents", "app": "opt-chat"}

func kindOf(key string) (notifyKind, bool) {
	i := slices.IndexFunc(notifyKinds, func(k notifyKind) bool { return k.Key == key })
	if i < 0 {
		return notifyKind{}, false
	}
	return notifyKinds[i], true
}

// notifyRule is one kind's switches.
type notifyRule struct {
	Sound bool `json:"sound"`
	Desk  bool `json:"desk"`
}

// notifySettings are the user's choices, in settings.json: each switch
// the user flipped, by kind, apart, so the other keeps its default.
type notifySettings struct {
	Sound   map[string]bool `json:"sound,omitzero"`
	Desk    map[string]bool `json:"desk,omitzero"`
	Muted   []string        `json:"muted,omitzero"`   // subagents told of by neither, nor their own
	Focused bool            `json:"focused,omitzero"` // notifications also while a page is in front
}

// rule is the switches of kind k.
func (n notifySettings) rule(k notifyKind) notifyRule {
	r := notifyRule{Sound: !k.Quiet, Desk: k.Desk}
	if v, ok := n.Sound[k.Key]; ok {
		r.Sound = v
	}
	if v, ok := n.Desk[k.Key]; ok {
		r.Desk = v
	}
	return r
}

// eventUpdate asks the pages for a desktop notification.
type eventUpdate struct {
	T       string `json:"t"`  // event
	ID      string `json:"id"` // the same for every page: one of them shows it
	Kind    string `json:"kind"`
	Who     string `json:"who,omitzero"` // the subagent's name; empty: Opt's or opt-chat's
	Title   string `json:"title"`
	Body    string `json:"body,omitzero"`
	Sound   bool   `json:"sound,omitzero"`   // opt-chat played its sound: the notification is silent
	Focused bool   `json:"focused,omitzero"` // shown even by a page in front
}

// notifyBody is the most of a text a notification shows.
const notifyBody = 180

// event tells of an event of kind ("done", "error", ...) by who: Opt if
// empty, else a subagent, of the kind's agent or sub part by whether it
// has a parent. A kind with its part ("app.start") is taken as it is. It
// plays the kind's sound and asks the pages for a notification, as the
// user's switches say; never waits, and takes no lock but the hub's.
func (h *hub) event(kind, who, body string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	key := kind
	if !strings.Contains(kind, ".") {
		switch {
		case who == "":
			key = "opt." + kind
		case h.parents[who] != "":
			key = "sub." + kind
		default:
			key = "agent." + kind
		}
	}
	k, ok := kindOf(key)
	if !ok {
		h.mu.Unlock()
		warn("hub: no kind of event %q", key)
		return
	}
	n := h.notify
	muted := who != "" && (slices.Contains(n.Muted, who) || slices.Contains(n.Muted, h.parents[who]))
	r := n.rule(k)
	h.evSeq++
	id := fmt.Sprintf("%d-%d", h.born, h.evSeq)
	h.mu.Unlock()
	if muted {
		return
	}
	if r.Sound {
		h.sounds.sound(k.Sound)
	}
	if r.Desk {
		title := k.Label
		switch {
		case strings.HasPrefix(key, "opt."):
			title = "Opt " + title
		case who != "":
			title = "[" + who + "] " + title
		default:
			title = "opt-chat " + title
		}
		h.send(eventUpdate{T: "event", ID: id, Kind: key, Who: who, Title: title, Body: oneLineN(body, notifyBody),
			Sound: r.Sound && h.sounds != nil, Focused: n.Focused})
	}
}

// oneLineN is s on one line, cut to n characters.
func oneLineN(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// setParent notes that subagent name is the subagent parent's own: its
// events are of their sub kinds.
func (h *hub) setParent(name, parent string) {
	if h == nil || parent == "" {
		return
	}
	h.mu.Lock()
	if h.parents == nil {
		h.parents = map[string]string{}
	}
	h.parents[name] = parent
	h.mu.Unlock()
}

func (h *hub) setNotify(n notifySettings) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.notify = n
	h.mu.Unlock()
}

// notifyRow is one kind as the page's panel lists it.
type notifyRow struct {
	Key   string `json:"key"`
	Who   string `json:"who"`
	Label string `json:"label"`
	notifyRule
}

type notifyReply struct {
	T       string      `json:"t"` // notify
	Action  string      `json:"action,omitzero"`
	Error   string      `json:"error,omitzero"`
	Notify  []notifyRow `json:"notify"`
	Muted   []string    `json:"muted"`
	Focused bool        `json:"focused"`
	Sounds  bool        `json:"sounds"` // opt-chat plays sounds: -sounds, and peon-ping is there
}

// notifyRequest answers the page's panel: the list, or a change to it, kept
// in settings.json. Actions: sound and desk flip kind name's switch, mute
// flips subagent name's, focused flips whether a page in front shows them.
func (a *Agent) notifyRequest(action, name string) notifyReply {
	h := a.chat.hub
	k, known := kindOf(name)
	var err error
	switch {
	case action == "" || action == "focused":
	case action == "sound" || action == "desk":
		if !known {
			err = fmt.Errorf("no kind of event %q", name)
		}
	case action == "mute":
		if name == "" {
			err = errors.New("mute whom?")
		}
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	if action != "" && err == nil {
		err = a.updateSettings(func(s *settings) {
			n := &s.Notify
			switch action {
			case "sound":
				if n.Sound == nil {
					n.Sound = map[string]bool{}
				}
				n.Sound[k.Key] = !n.rule(k).Sound
			case "desk":
				if n.Desk == nil {
					n.Desk = map[string]bool{}
				}
				n.Desk[k.Key] = !n.rule(k).Desk
			case "mute":
				if i := slices.Index(n.Muted, name); i >= 0 {
					n.Muted = slices.Delete(n.Muted, i, i+1)
				} else {
					n.Muted = append(n.Muted, name)
				}
			case "focused":
				n.Focused = !n.Focused
			}
			h.setNotify(*n)
		})
	}
	h.mu.Lock()
	n := h.notify
	sounds := h.sounds != nil
	h.mu.Unlock()
	rep := notifyReply{T: "notify", Action: action, Muted: slices.Clone(n.Muted), Focused: n.Focused, Sounds: sounds}
	if rep.Muted == nil {
		rep.Muted = []string{}
	}
	if err != nil {
		rep.Error = err.Error()
	}
	for _, k := range notifyKinds {
		rep.Notify = append(rep.Notify, notifyRow{Key: k.Key, Who: notifyWho[strings.Split(k.Key, ".")[0]], Label: k.Label, notifyRule: n.rule(k)})
	}
	return rep
}

// lastReply is the text of Opt's last reply among the messages from from
// on, a turn's, for the notification of its end; "" when it wrote none.
func (c *Chat) lastReply(from int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := len(c.msgs) - 1; i >= max(from, 0); i-- {
		if c.msgs[i].Kind == "opt" {
			return c.msgs[i].Text
		}
	}
	return ""
}
