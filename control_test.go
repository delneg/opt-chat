package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Claude Code's lists come from one call that takes control requests only:
// started once, answering each request by its id, its errors as errors.
func TestControl(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, calls := fakeAgent(t, c)
	ctx := context.Background()
	ms, err := a.models(ctx)
	if err != nil || len(ms) != 3 || ms[1].Value != "big" || !slices.Equal(ms[1].Efforts, []string{"low", "high"}) || !ms[1].SupportsEffort {
		t.Fatalf("models: %+v, %v", ms, err)
	}
	if _, err := a.control(ctx, map[string]any{"subtype": "fail"}); err == nil || err.Error() != "no such thing" {
		t.Fatalf("a failed request: %v", err)
	}
	if _, err := a.control(ctx, map[string]any{"subtype": "mcp_status"}); err != nil {
		t.Fatal(err)
	}
	a.models(ctx) // asked once a run
	if got := calls(); len(got) != 1 || slices.Contains(got[0], "--include-partial-messages") || slices.Contains(got[0], "--effort") ||
		flagOf(got[0], "--model") != "big" {
		t.Fatalf("calls: %q", got)
	}
	// Once it has exited, the next request starts another.
	a.ctlMu.Lock()
	a.ctl.p.close()
	a.ctlMu.Unlock()
	waitFor(t, "the call to end", func() bool { a.ctlMu.Lock(); defer a.ctlMu.Unlock(); return a.ctl == nil })
	if _, err := a.control(ctx, map[string]any{"subtype": "mcp_status"}); err != nil || len(calls()) != 2 {
		t.Fatalf("after an exit: %v, %d calls", err, len(calls()))
	}
}

// The picker's choice holds for the run, and with save for later runs too.
func TestChoose(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	if err := a.choose("opus", "max", false); err != nil || a.model != "opus" || a.effort != "max" {
		t.Fatalf("%v: %s %s", err, a.model, a.effort)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("saved a choice for this run only")
	}
	if err := a.choose("sonnet", "low", true); err != nil {
		t.Fatal(err)
	}
	if s := loadSettings(c.dir); s.Model != "sonnet" || s.Effort != "low" || s.MCPOff != nil {
		t.Fatalf("saved %+v", s)
	}
	for _, bad := range [][2]string{{"two words", "high"}, {"opus", "huge"}, {"opus", ""}} {
		if err := a.choose(bad[0], bad[1], true); err == nil {
			t.Fatalf("%q taken", bad)
		}
	}
	if a.model != "sonnet" || a.effort != "low" || !strings.Contains(string(must(os.ReadFile(filepath.Join(c.dir, "settings.json")))), `"sonnet"`) {
		t.Fatal("a bad choice changed something")
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
