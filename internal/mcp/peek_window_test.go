package mcp

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// The window is a PEEK BUDGET, not a wait for completion: a caller looks at what a
// long command has said so far and comes back later. Two things break that contract,
// and both are in the wait rather than in the reply.

// TestAFullWindowEndsTheWait pins the one that wastes the caller's time: when the reply
// cannot grow any further, waiting for the rest of the window buys nothing.
//
// A noisy command (a scan, a brute force) never goes quiet, so the quiet detector never
// fires and the wait ran to the caller's full window — while the reply could never hold
// more than `bytes_limit`. The reported symptom was a client-side timeout on a 320s
// window over a command that had already filled the reply in the first second.
func TestAFullWindowEndsTheWait(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// Prints steadily for longer than the window: never quiet, never done.
	noisy := "for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do echo line $i; sleep 0.2; done"
	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":         "run " + h + " $1",
		"args":        []any{noisy},
		"bytes_limit": 200,
		"timeout":     10,
	})
	elapsed := time.Since(start)

	if !strings.Contains(got, "line") {
		t.Fatalf("the peek should have caught output:\n%s", got)
	}
	// 200 bytes fills in well under a second; 10s is the caller's window, not a target.
	if elapsed > 3*time.Second {
		t.Errorf("a full byte window should end the wait; waited %v of a 10s window", elapsed)
	}
	// And the caller has to be told the command is not done, or "here is a slice" and
	// "here is everything" look the same.
	if !strings.Contains(got, "Still-Running: true") {
		t.Errorf("a window cut short by its own size must say the command is still running:\n%s", got)
	}
}

// TestWaitedCoversTheWholeCall pins the header that reported the wrong number.
//
// `Waited` is how a caller tells "I waited and heard nothing" from "I did not wait at
// all" — the distinction that keeps a peek from looking like a broken channel. It was
// measured from the READ, so the wait before it (which is where the window was spent)
// was missing: a call that spent a full second waiting answered `Waited: 0ms`, and the
// reporting caller read that as "the window never happened".
func TestWaitedCoversTheWholeCall(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// THE ASSERTION IS "THE WAIT IS IN THERE", NOT "IT EQUALS THE WINDOW".
	//
	// A 1s window is no longer a promise of 1000ms: a pty wait ends when the stream goes
	// still (issue #156), and the echo of the typed command is output — so a legal reply
	// can come back at 600ms. Pinning 900ms made this test fail on a loaded machine for
	// doing the right thing. What the header has to satisfy is that it covers the whole
	// CALL: the bug was measuring it from the read alone, which reported ~0ms while the
	// call spent its window waiting. So the yardstick is the call's own duration.
	start := time.Now()
	got := termText(t, s, map[string]any{
		"cmd":     "run " + h + " $1",
		"args":    []any{"sleep 2"},
		"timeout": 1,
	})
	elapsed := time.Since(start)
	waited := headerValueOr(got, "Waited", "")
	if waited == "" {
		t.Fatalf("a call that waited has to say how long:\n%s", got)
	}
	ms, err := strconv.Atoi(strings.TrimSuffix(waited, "ms"))
	if err != nil {
		t.Fatalf("Waited is not a duration: %q", waited)
	}
	// Slack for the transport either side of the server's own clock; the failure this
	// guards against was off by the entire wait, not by a millisecond.
	if slack := int(elapsed.Milliseconds()) - 250; ms < slack {
		t.Errorf("Waited reports %dms for a call that took %dms: the wait is missing from it", ms, elapsed.Milliseconds())
	}
}
