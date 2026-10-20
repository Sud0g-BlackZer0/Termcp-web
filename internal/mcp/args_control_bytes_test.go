package mcp

import (
	"strings"
	"testing"
)

// TestControlBytesInValuesAreRefused pins the gate that cmd-only scanning missed.
//
// A value reaches the terminal without appearing in cmd, so scanning cmd alone let
// `args: ["a\x03b"]` through. What the terminal then did was worse than a missing
// check: it acted on the byte, echoed `^C`, and the shell dropped into its `quote>`
// continuation prompt — while the reply still said 200.
//
// Quoting is no defence. A control byte inside single quotes is still a control byte
// to the terminal, which is exactly why the gate has to look at the VALUE.
func TestControlBytesInValuesAreRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	cases := []struct {
		name  string
		args  map[string]any
		where string
	}{
		{
			"ctrl-c in args",
			map[string]any{"cmd": "run " + h + " echo $1", "args": []any{"a\x03b"}},
			"args[0]",
		},
		{
			"carriage return in args",
			map[string]any{"cmd": "run " + h + " echo $1", "args": []any{"a\rb"}},
			"args[0]",
		},
		{
			"ctrl-c in vars",
			map[string]any{"cmd": "run " + h + " echo $V", "vars": map[string]any{"V": "a\x03b"}},
			"vars[V]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := termText(t, s, tc.args)
			if !strings.HasPrefix(got, "TERM/1 4") {
				t.Fatalf("a control byte must be refused, got:\n%s", got)
			}
			if !strings.Contains(got, tc.where) {
				t.Errorf("the refusal should name where the byte is (%q):\n%s", tc.where, got)
			}
		})
	}
}

// TestControlBytesPassWhenAllowed pins that allow_escapes is still the way out: the
// gate reports, it does not decide for the caller who meant the byte.
func TestControlBytesPassWhenAllowed(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{
		"cmd": "run " + h + " echo $1", "args": []any{"x\x03y"}, "allow_escapes": true,
	})
	if !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("allow_escapes must let the byte through:\n%s", got)
	}
}
