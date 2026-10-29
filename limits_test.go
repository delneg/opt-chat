package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The plan's limits: the five-hour and seven-day windows from every call's
// rate_limit_event, every window from get_usage, asked at most once a
// minute; the bar shows those two always, any other only while in use, and
// one past its reset at 0.
func TestLimits(t *testing.T) {
	t.Parallel()
	c, _ := fakeChat(t, t.TempDir(), 100)
	a, _ := fakeAgent(t, c)
	now := time.Unix(1_800_000_000, 0)
	show := func() string {
		var out []string
		for _, w := range a.windows(now) {
			s := fmt.Sprintf("%s %.0f%%", w.Name, w.Used)
			if w.Limit > 0 {
				s += fmt.Sprintf(" $%.0f/$%.0f", w.Spent, w.Limit)
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}
	if got := show(); got != "" {
		t.Fatalf("before any call: %q", got)
	}
	if err := (&Turn{agent: a}).Run(context.Background(), []message{{kind: "user", text: "stream-reply"}}); err != nil {
		t.Fatal(err)
	}
	if got := show(); got != "five_hour 9%, seven_day 31%" {
		t.Fatalf("from the turn's event: %q", got)
	}
	ctx := context.Background()
	if err := a.askUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if got := show(); got != "five_hour 12%, seven_day 33%, iguana_necktie 0% $20/$250, seven_day_fable 40%" {
		t.Fatalf("from get_usage: %q", got)
	}
	if err := a.askUsage(ctx); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(fakeFiles(a, ".ctl"), "\n"), `"get_usage"`); n != 1 {
		t.Fatalf("get_usage asked %d times within a minute", n)
	}
	now = time.Unix(1_800_020_000, 0) // past the five-hour window's reset
	if got := show(); got != "five_hour 0%, seven_day 33%, iguana_necktie 0% $20/$250, seven_day_fable 40%" {
		t.Fatalf("after a reset: %q", got)
	}
}
