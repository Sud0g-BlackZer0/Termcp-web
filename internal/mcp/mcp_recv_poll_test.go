package mcp

import (
	"strings"
	"testing"
)

// TestRecvWaitsForTheFrameInsteadOfSamplingOnce pins the poll budget.
//
// shell_output honours `timeout` only in its live-cursor mode; an offset-addressed
// read returns what is buffered NOW and nothing else. A recv that asked once
// therefore answered "no complete frame yet" while the peer's bytes were still in
// flight, and the caller had to guess how long to keep asking — the directive's own
// help says "poll rather than treating that as an error", which is advice about the
// implementation leaking into the protocol.
//
// One recv now means one frame: it retries within a bounded budget, so a peer that
// answers in tens of milliseconds (or a frame that arrives while the read is being
// set up) is delivered rather than reported absent.
func TestRecvWaitsForTheFrameInsteadOfSamplingOnce(t *testing.T) {
	s := newTestServer(t)

	// A frame that is not there at the instant of the read: the peer sleeps first.
	h := openPipe(t, s, `sh -c 'sleep 0.3; printf "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n"'`)
	got := termText(t, s, map[string]any{"cmd": "mcp recv " + h})
	if strings.Contains(got, "no complete frame") {
		t.Errorf("recv sampled once instead of waiting:\n%s", got)
	}
	if !strings.Contains(got, "ok") {
		t.Errorf("recv did not deliver the frame:\n%s", got)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestRecvDeliversAFrameLargerThanTheReadWindow pins the other half: the window is a
// cap on what one read considers, not a statement about how long a frame may be.
func TestRecvDeliversAFrameLargerThanTheReadWindow(t *testing.T) {
	s := newTestServer(t)
	payload := strings.Repeat("x", 10000)
	h := openPipe(t, s, `sh -c 'printf "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"big\":\"`+payload+`\"}}\n"'`)

	// Wait for the writer to finish before reading, so the frame is complete in the
	// buffer. Generating and piping 10 KB of JSON through sh takes longer than the
	// recv retry budget on a loaded CI runner (and longer again on Windows inside a
	// VM), and the read then samples mid-frame: the failure is `bytes 0-72/73`, a
	// partial first chunk, not a window that is too small.
	waitForShellToExit(t, s, shellIDFromHandle(t, s, h))

	got := termText(t, s, map[string]any{"cmd": "mcp recv " + h})
	if !strings.Contains(got, payload) {
		t.Errorf("a 10 KB frame was not delivered (window should be 1 MiB):\n%.300s", got)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestRecvStillReportsAbsenceWhenNothingArrives pins that the budget is a bound, not
// a hang: an empty channel still answers 204 after it.
func TestRecvStillReportsAbsenceWhenNothingArrives(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, `sh -c 'sleep 10'`)
	got := termText(t, s, map[string]any{"cmd": "mcp recv " + h})
	if !strings.Contains(got, "no complete frame") {
		t.Errorf("an empty channel should report absence:\n%s", got)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestRecvFindsAFrameBehindStartupNoise pins the parse that a stdio peer's own
// output was breaking.
//
// A pipe channel carries the PEER'S stderr banner and npm warnings on the same
// stream as the protocol. Measured on @modelcontextprotocol/server-everything: 149
// bytes of "npm warn Unknown user config ..." and "Starting default (STDIO)
// server..." before the first '{'. Every recv on that channel answered "no complete
// frame yet" while a complete frame sat in the buffer, because the decoder was
// handed the warning instead of the frame.
//
// A JSON value starts at '{' or '['. Nothing before the first one can be part of it,
// so skipping to that byte is what the line-delimited reality of a pipe channel
// needs — and it is why this is a scan rather than a TrimSpace.
func TestRecvFindsAFrameBehindStartupNoise(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, `sh -c 'printf "npm warn Unknown user config\nStarting default (STDIO) server...\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n"; sleep 3'`)
	got := termText(t, s, map[string]any{"cmd": "mcp recv " + h})
	if strings.Contains(got, "no complete frame") {
		t.Fatalf("a frame behind startup noise was reported absent:\n%s", got)
	}
	if !strings.Contains(got, `"ok":true`) {
		t.Errorf("the wrong bytes were delivered:\n%s", got)
	}
	// The cursor advanced past the frame, not past the noise+frame.
	if !strings.Contains(got, "200 OK") {
		t.Errorf("recv should succeed:\n%s", got)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}
