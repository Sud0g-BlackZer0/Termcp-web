package mcp

import (
	"strings"
	"testing"
)

// TestADialTimeoutOutsideTheRangeIsRefused.
//
// The dialler clamps the timeout to [30,120], so a profile saying 5 was stored, echoed back
// by `get host`, and then dialled at 30 seconds: the number the caller set and the number in
// force were different, and nothing said so. Silently clamping is the failure this repo
// keeps closing — the value is accepted, appears in the config, and is not what runs.
func TestADialTimeoutOutsideTheRangeIsRefused(t *testing.T) {
	s := newTestServer(t)
	s.ManageSSHConfigs = true

	// A SHORT TIMEOUT IS LEGAL, and that is the half this test exists for after the first
	// version got it wrong: 30 is the DEFAULT, not a floor, so a caller that wants a 5
	// second dial to fail fast can have one. Only a negative value or one above the cap is
	// refused, because those are the ones the dialler would silently replace.
	for _, tc := range []struct{ seconds, want string }{
		{"-1", "must be 0 (the default, 30s) or between 1 and 120"},
		{"600", "must be 0 (the default, 30s) or between 1 and 120"},
	} {
		got := termText(t, s, map[string]any{
			"cmd": "create host slow --host 192.0.2.1 --user u --password p --dial-timeout-seconds " + tc.seconds,
		})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("a %ss dial timeout was accepted:\n%s", tc.seconds, got)
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("the refusal does not name the range for %s:\n%s", tc.seconds, got)
		}
	}

	// The edges, the default and a SHORT value all stay legal.
	for _, ok := range []string{"0", "1", "5", "30", "120"} {
		got := termText(t, s, map[string]any{
			"cmd": "create host ok" + ok + " --host 192.0.2.1 --user u --password p --dial-timeout-seconds " + ok,
		})
		if strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("a %ss dial timeout was refused:\n%s", ok, got)
		}
	}
}

// TestTheClampIsTheValidator is the one-owner check: the bound the dialler applies and the
// bound the validator refuses on have to be the same number, or a value can pass validation
// and still be replaced at dial time.
func TestTheClampIsTheValidator(t *testing.T) {
	s := newTestServer(t)
	s.ManageSSHConfigs = true
	got := termText(t, s, map[string]any{
		"cmd": "create host clamped --host 192.0.2.1 --user u --password p --dial-timeout-seconds 31",
	})
	if strings.HasPrefix(got, "TERM/1 4") {
		t.Fatalf("a value inside the clamp's range was refused:\n%s", got)
	}
	// The stored value has to be the one the caller set: a validator that accepts 31 and
	// then lets the dialler use 30 would be the same silent substitution, one step later.
	back := termText(t, s, map[string]any{"cmd": "get host clamped"})
	if !strings.Contains(back, "31") {
		t.Errorf("the stored timeout is not the one that was set: %s", back)
	}
}
