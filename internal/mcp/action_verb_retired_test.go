package mcp

import (
	"strings"
	"testing"
)

// TestActionVerbIsRetiredAndSaysWhereItWent pins the removal of --action-verb.
//
// IT WAS DOCUMENTED AS A WORKING ALIAS AND IT NEVER WORKED. The directive declares
// the flags it accepts and the parser refuses the rest before any handler runs, so
// `--action-verb` died with a bare "unknown flag" while ops_detail.go told the
// caller it was "the older spelling of --op and does the same thing; --op wins if
// both are given". A fallback in term_generated.go read stmt.Flag("action-verb")
// that could not be reached. Both are gone now: the name sits in RetiredFlags, and
// the refusal names the flag that replaced it.
//
// The DSL has no callers to migrate — an AI reads the reference each turn and
// writes against it — so there is no reason to keep a spelling alive for
// compatibility. What a retired flag owes the caller is a sentence, not a shim.
func TestActionVerbIsRetiredAndSaysWhereItWent(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sid, _, _ := strings.Cut(h, ":")

	for _, cmd := range []string{
		"get " + sid + " file /tmp/x --action-verb stat",
		"write " + sid + " file /tmp/x --data hi --action-verb chmod",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Fatalf("%q must be refused with a 4xx, got:\n%s", cmd, got)
		}
		// A BARE "unknown flag" IS NOT ENOUGH. The caller wrote a name that used to
		// be documented; the answer has to say where it went, or it reads as a typo.
		if !strings.Contains(got, "--op") {
			t.Errorf("%q was refused without naming --op:\n%s", cmd, got)
		}
		if !strings.Contains(got, "gone") {
			t.Errorf("%q was refused without saying the flag is retired:\n%s", cmd, got)
		}
	}

	// The surviving spelling must still work, so this test cannot pass on a build
	// that refuses both names.
	ok := termText(t, s, map[string]any{"cmd": "get " + sid + " file /tmp --op stat"})
	if strings.Contains(ok, "unknown flag") {
		t.Fatalf("--op must still be accepted:\n%s", ok)
	}
}
