package main

import (
	"encoding/json/v2"
	"sync"
	"testing"
)

// Turn updates reach a page in the order they were made: the last one it
// gets is the turn as the hub has it, however many set it at once (a turn's
// phase, and a message sent in as a tool call starts).
func TestTurnUpdatesInOrder(t *testing.T) {
	t.Parallel()
	for round := range 300 {
		h := newHub()
		cl := &client{out: make(chan []byte, outbox)}
		h.clients[cl] = true
		var wg sync.WaitGroup
		for g := range 4 {
			wg.Go(func() {
				for i := range 50 {
					h.setTurn(func(lt *liveTurn) { lt.Waiting = 1 + g*100 + i })
				}
			})
		}
		wg.Wait()
		var last liveTurn
		for len(cl.out) > 0 {
			var u turnUpdate
			if err := json.Unmarshal(<-cl.out, &u); err != nil {
				t.Fatal(err)
			}
			last = u.Turn
		}
		if last.Waiting != h.turn.Waiting {
			t.Fatalf("round %d: the page's last turn update has waiting %d, the hub %d", round, last.Waiting, h.turn.Waiting)
		}
	}
}
