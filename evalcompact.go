package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"sync"
)

// runEvalCompact builds lines of the tree again, as a compaction would build
// them now, without keeping them: for each JSON line of in, {"l", "i", "runs"},
// it runs the compaction of node (l, i) runs times, each with the context
// and task that node would get, and writes each line it gets to out, next to
// the line the tree holds and the task's text. It measures what a change to
// the compaction task or model does to real lines (see
// docs/2026-10-23-faithful-lines.md); nothing in the chat changes.
func runEvalCompact(a *Agent, in, out string, par int) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	type job struct {
		L, I, Runs int
	}
	var jobs []job
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var j struct {
			L    int `json:"l"`
			I    int `json:"i"`
			Runs int `json:"runs"`
		}
		if err := json.Unmarshal(sc.Bytes(), &j); err != nil {
			return fmt.Errorf("%s: %v", in, err)
		}
		jobs = append(jobs, job{j.L, j.I, max(j.Runs, 1)})
	}
	if err := sc.Err(); err != nil {
		return err
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	c := a.chat
	var mu sync.Mutex
	sem := make(chan struct{}, max(par, 1))
	var wg sync.WaitGroup
	for _, j := range jobs {
		k := key{j.L, j.I}
		c.mu.Lock()
		if (k.L == 0 && k.I >= len(c.msgs)) || (k.L > 0 && !c.built(k)) {
			c.mu.Unlock()
			return fmt.Errorf("no line %s to build again", k.name())
		}
		ctxLines, stable, task := c.compactInput(k)
		old := c.tree[k]
		c.mu.Unlock()
		var input strings.Builder
		for _, b := range task {
			input.WriteString(b.Text)
		}
		for run := range j.Runs {
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				ctx, cancel := context.WithTimeout(context.Background(), compactTimeout)
				line, err := a.Compact(ctx, ctxLines, stable, task)
				cancel()
				r := map[string]any{"l": k.L, "i": k.I, "run": run, "line": line, "old": old, "input": input.String()}
				if err != nil {
					r["err"] = err.Error()
				}
				b, _ := json.Marshal(r)
				mu.Lock()
				defer mu.Unlock()
				w.Write(append(b, '\n')) // an error shows at Close
				dim(fmt.Sprintf("[eval-compact %s run %d] %d bytes", k.name(), run+1, len(line)))
			})
		}
	}
	wg.Wait()
	n, u := a.takeCompactions()
	dim(fmt.Sprintf("[eval-compact] %d compactions: input %d, cache write %d, cache read %d, output %d tokens", n, u.Input, u.Write, u.Read, u.Output))
	b, _ := json.Marshal(map[string]any{"usage": u, "compactions": n, "model": a.cmodel})
	w.Write(append(b, '\n'))
	return w.Close()
}
