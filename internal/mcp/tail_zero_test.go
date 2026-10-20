package mcp

import (
	"strings"
	"testing"
)

// TestTailZeroReturnsEmptyWindow pins that `--tail 0` returns zero lines rather than
// the entire buffer, and that `--tail 1` still returns the last line.
func TestTailZeroReturnsEmptyWindow(t *testing.T) {
	s := newTestServer(t)

	// A pipe that prints three lines and exits, so the log is complete and stable
	// by the time we read it.
	text := termText(t, s, map[string]any{
		"cmd": "pipe internal --argv printf 'LINE_ONE\\nLINE_TWO\\nLINE_THREE\\n'",
	})
	h := handleFrom(t, text)

	// Wait for the process to finish so its bytes are in the log.
	termText(t, s, map[string]any{"cmd": "read " + h + " --from-line 1"})

	// Tail 0: an empty window, not the whole log.
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --tail 0"})
	if !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("expected 204 for --tail 0, got:\n%s", got)
	}
	for _, bad := range []string{"LINE_ONE", "LINE_TWO", "LINE_THREE"} {
		if strings.Contains(got, bad) {
			t.Errorf("--tail 0 must not return log content (%s): %s", bad, got)
		}
	}

	// Tail 1: exactly the last line.
	got = termText(t, s, map[string]any{"cmd": "read " + h + " --tail 1"})
	if !strings.Contains(got, "LINE_THREE") {
		t.Errorf("--tail 1 should contain the last line: %s", got)
	}
	if strings.Contains(got, "LINE_ONE") {
		t.Errorf("--tail 1 must not contain the first line: %s", got)
	}
}
