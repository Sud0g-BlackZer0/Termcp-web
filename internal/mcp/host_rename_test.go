package mcp

import (
	"strings"
	"testing"
)

// TestHostDirectiveWorksUnderBothNames pins that `host` is the canonical name and
// `profile` is its transparent synonym.
func TestHostDirectiveWorksUnderBothNames(t *testing.T) {
	s := newTestServer(t)

	// Both `host list` and `profile list` must return 200 with the host list.
	underHost := termText(t, s, map[string]any{"cmd": "host list"})
	if !strings.HasPrefix(underHost, "TERM/1 200") {
		t.Fatalf("host list failed:\n%s", underHost)
	}

	underProfile := termText(t, s, map[string]any{"cmd": "profile list"})
	if !strings.HasPrefix(underProfile, "TERM/1 200") {
		t.Fatalf("profile list failed:\n%s", underProfile)
	}

	// Canonical name is host: `help host` and `help profile` must give the same
	// information.
	helpHost := termText(t, s, map[string]any{"cmd": "help host"})
	helpProfile := termText(t, s, map[string]any{"cmd": "help profile"})
	// The Path header echoes what was typed, so compare the bodies: the point is that
	// both names resolve to ONE directive, not that the echo is identical.
	bodyOf := func(s string) string {
		if i := strings.Index(s, "\n\n"); i >= 0 {
			return s[i:]
		}
		return s
	}
	if bodyOf(helpHost) != bodyOf(helpProfile) {
		t.Errorf("help host and help profile must teach the same directive:\n--- host:\n%s\n--- profile:\n%s", helpHost, helpProfile)
	}
	if !strings.Contains(helpHost, "host list|create") {
		t.Errorf("usage should show host, not profile:\n%s", helpHost)
	}
}
