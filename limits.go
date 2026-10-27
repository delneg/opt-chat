package main

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
	"time"
)

// The plan's usage limits, as Claude Code's status line shows them: every
// call reports the five-hour and seven-day windows as it starts
// (rate_limit_event), and get_usage, which the page asks after each turn,
// tells every window, those of a model or of credits too. The page's bar
// shows them.

// window is one usage limit.
type window struct {
	Name   string  `json:"name"`            // five_hour, seven_day, seven_day_opus, or another of Claude Code's
	Used   float64 `json:"used"`            // percent
	Resets int64   `json:"resets,omitzero"` // unix seconds
	Limit  float64 `json:"limit,omitzero"`  // in dollars, for a window counted in them
	Spent  float64 `json:"spent,omitzero"`
}

// limitInfo is a rate_limit_event's. Status is "rejected" while the plan
// refuses requests, until ResetsAt, unless usage credits pay for them
// (OverageStatus "allowed" or "allowed_warning").
type limitInfo struct {
	Status        string `json:"status"` // allowed, allowed_warning or rejected
	ResetsAt      int64  `json:"resetsAt,omitzero"`
	RateLimitType string `json:"rateLimitType,omitzero"` // five_hour, seven_day, seven_day_opus, ...
	OverageStatus string `json:"overageStatus,omitzero"`
	ErrorCode     string `json:"errorCode,omitzero"` // credits_required: only bought credits lift it, no reset does
	Windows       map[string]struct {
		Utilization float64 `json:"utilization"` // a fraction
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

const usageEvery = time.Minute // get_usage at most this often

func (a *Agent) noteLimits(l limitInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.Status != "" {
		a.refused = l
	}
	for name, w := range l.Windows {
		x := a.limits[name]
		x.Name, x.Used, x.Resets = name, w.Utilization*100, w.ResetsAt
		a.setLimit(x)
	}
}

func (a *Agent) setLimit(w window) {
	if a.limits == nil {
		a.limits = map[string]window{}
	}
	a.limits[w.Name] = w
}

// askUsage asks Claude Code for every window, at most once a minute.
func (a *Agent) askUsage(ctx context.Context) error {
	a.mu.Lock()
	if time.Since(a.usageAsked) < usageEvery {
		a.mu.Unlock()
		return nil
	}
	a.usageAsked = time.Now()
	a.mu.Unlock()
	v, err := a.control(ctx, map[string]any{"subtype": "get_usage"})
	if err != nil {
		return err
	}
	var r struct {
		Limits map[string]jsontext.Value `json:"rate_limits"`
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planRaw = v
	for name, raw := range r.Limits {
		var w struct { // null for a window the plan has not
			Utilization *float64 `json:"utilization"` // a percent
			ResetsAt    string   `json:"resets_at"`
			Limit       float64  `json:"limit_dollars"`
			Used        float64  `json:"used_dollars"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		x := window{Name: name, Used: *w.Utilization, Limit: w.Limit, Spent: w.Used}
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			x.Resets = t.Unix()
		}
		a.setLimit(x)
	}
	return nil
}

// windows are the limits to show: the five-hour and seven-day ones first,
// then the others in use; one past its reset is at 0 again.
func (a *Agent) windows(now time.Time) []window {
	a.mu.Lock()
	defer a.mu.Unlock()
	rank := func(w window) int { return cmp.Or(map[string]int{"five_hour": 1, "seven_day": 2}[w.Name], 3) }
	var out []window
	for _, w := range slices.Collect(maps.Values(a.limits)) {
		if w.Resets > 0 && w.Resets <= now.Unix() {
			w.Used, w.Spent = 0, 0
		}
		if rank(w) < 3 || w.Used > 0 || w.Spent > 0 {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(x, y window) int { return cmp.Or(cmp.Compare(rank(x), rank(y)), cmp.Compare(x.Name, y.Name)) })
	return out
}
