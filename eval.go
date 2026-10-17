package main

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// -eval asks a file of questions about the chat, each as a side question
// (/btw) with no earlier side questions, and writes each answer with what
// the call cost and which tools it called. Nothing joins the chat or btw/.
// It is how the chat's memory gets measured: run it on a copy of the
// chat, once per setting to compare, and grade the answers against the
// questions' gold answers (docs/2026-10-17-memory-eval.md). After the
// memory benchmarks' question sets (LongMemEval,
// https://arxiv.org/abs/2410.10813; BEAM, https://arxiv.org/abs/2510.27246),
// but with questions drawn from the user's own chat.

// recallIDs are the ids of the messages a Recall block holds.
var recallIDs = regexp.MustCompile(`(?m)^(\d+)\+1\|`)

// runEval answers each line of in, a JSON object whose "q" is the question,
// and writes the object to out with the answer added, par at a time.
func runEval(a *Agent, in, out string, par int) error {
	f, err := os.Open(in)
	if err != nil {
		return err
	}
	defer f.Close()
	var qs []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var q map[string]any
		if err := json.Unmarshal(sc.Bytes(), &q); err != nil {
			return fmt.Errorf("%s: %v", in, err)
		}
		if _, ok := q["q"].(string); !ok {
			return fmt.Errorf("%s: a line has no \"q\"", in)
		}
		qs = append(qs, q)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	defer w.Close()

	c := a.chat
	c.mu.Lock()
	lines, before := c.callView(), len(c.msgs)
	c.mu.Unlock()
	var mu sync.Mutex
	sem := make(chan struct{}, max(par, 1))
	var wg sync.WaitGroup
	for n, q := range qs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			text := q["q"].(string)
			var ids []int
			for _, m := range recallIDs.FindAllStringSubmatch(c.Recall(text, before, a.recall), -1) {
				id, _ := strconv.Atoi(m[1])
				ids = append(ids, id)
			}
			start := time.Now()
			r, err := a.btw(lines, nil, message{kind: "user", text: text}, before)
			q["answer"], q["tools"], q["recalled"] = r.answer, r.tools, ids
			q["cost"], q["requests"], q["secs"] = r.result.Cost, r.result.Turns, time.Since(start).Seconds()
			if err != nil {
				q["err"] = err.Error()
			}
			b, _ := json.Marshal(q)
			mu.Lock()
			defer mu.Unlock()
			w.Write(append(b, '\n'))
			dim(fmt.Sprintf("[eval %d/%d] %s: %d tools, $%.3f", n+1, len(qs), oneLine(text, 60), len(r.tools), r.result.Cost))
		})
	}
	wg.Wait()
	return nil
}
