package main

import (
	"strings"
	"testing"
)

// The owed list: Opt adds an item only citing a user's message, a subagent
// none; only the user's /done closes one; each turn opens with the list,
// and it outlasts a restart.
func TestOwed(t *testing.T) {
	c, _ := fakeChat(t, t.TempDir(), 100)
	c.Log("user", "make the build green")
	c.Log("opt", "on it")
	a := &Agent{chat: c, running: map[string]*proc{}, writing: map[string]chan struct{}{}}
	off := len(a.mcpList())
	call := func(fromAgent bool, x toolArgs) string { s, _ := callTool(a, fromAgent, "owe", x); return s }
	if got := call(false, toolArgs{From: 0, Text: "x"}); got != "The owed list is off: opt-chat runs with -owed=false." {
		t.Fatalf("off: %q", got)
	}
	o, err := loadOwed(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	a.owed = o
	if n := len(a.mcpList()); n != off+1 {
		t.Fatalf("tools when on: %d, off: %d", n, off)
	}
	if a.owedNote() != nil {
		t.Fatal("an empty list opened a turn")
	}
	if got := call(false, toolArgs{From: 1, Text: "make it green"}); !strings.Contains(got, "no message of the user's") {
		t.Fatalf("cited Opt's own message: %q", got)
	}
	if got := call(false, toolArgs{From: 9, Text: "x"}); !strings.Contains(got, "no message of the user's") {
		t.Fatalf("cited no message: %q", got)
	}
	if got := call(true, toolArgs{From: 0, Text: "x"}); got != "Only Opt keeps the owed list." {
		t.Fatalf("a subagent's: %q", got)
	}
	if got := call(false, toolArgs{From: 0, Text: " make the build green "}); got != "Owed #1 added (asked in 0)." {
		t.Fatalf("add: %q", got)
	}
	if got := call(false, toolArgs{}); got != "#1 (asked in 0): make the build green" {
		t.Fatalf("list: %q", got)
	}
	if note := a.owedNote(); len(note) != 1 || !strings.Contains(note[0].Text, "closed only by the user:\n#1 (asked in 0): make the build green") {
		t.Fatalf("turn note: %v", note)
	}
	again, err := loadOwed(c.dir)
	if err != nil || again.text() != o.text() {
		t.Fatalf("after a restart: %q, %v", again.text(), err)
	}
	if got, _, _ := command(a, "/done 2"); got != "No owed item #2." {
		t.Fatalf("done 2: %q", got)
	}
	if got, _, _ := command(a, "/done x"); !strings.Contains(got, "takes an item's number") {
		t.Fatalf("done x: %q", got)
	}
	if got, _, _ := command(a, "/done #1"); got != "Closed #1: make the build green" {
		t.Fatalf("done 1: %q", got)
	}
	if got, _, _ := command(a, "/owed"); got != "Nothing owed." {
		t.Fatalf("owed after done: %q", got)
	}
}
