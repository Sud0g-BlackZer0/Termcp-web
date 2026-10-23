package directive

import (
	"strings"
	"testing"
)

// TestTheHolePrefixNeverCollides pins the invariant the fill pass depends on.
//
// `expand` finds holes by scanning the source for the prefix, so a prefix that appears
// in the caller's bytes would make the scan read THEIR text as a hole. Randomness does
// not fix that — it only makes it unlikely — so the check is the property that matters
// and it is tested directly, because no test can force a random prefix to collide.
func TestTheHolePrefixNeverCollides(t *testing.T) {
	cases := []struct {
		name string
		src  string
		seed string
	}{
		{"a clean source takes the seed as-is", "write h file __HOLE__", "__termcp_abc_"},
		{"a source that already holds it is extended", "x __termcp_abc_ y", "__termcp_abc_"},
		{"and again, because one underscore is not enough", "__termcp_abc_ and ___termcp_abc_", "__termcp_abc_"},
		{"the bare marker is the fallback seed", "a __termcp_ b", "__termcp_"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := uniquePrefix(tc.src, tc.seed)
			if strings.Contains(tc.src, got) {
				t.Errorf("uniquePrefix returned %q, which the source already contains: %q", got, tc.src)
			}
			// It must still carry the seed, or the token stops looking like a hole and
			// `isSentinel` cannot recognize a leak.
			if !strings.Contains(got, tc.seed) {
				t.Errorf("uniquePrefix returned %q, which no longer contains the seed %q", got, tc.seed)
			}
		})
	}
}

// TestASourceFullOfTheMarkerStillParses pins the collision path end to end: a caller
// whose cmd carries marker text still gets its values filled, because the prefix was
// extended rather than reused.
func TestASourceFullOfTheMarkerStillParses(t *testing.T) {
	// The marker appears in the cmd itself, in a position the fill pass scans. A naive
	// fixed prefix would find that text and read it as a hole.
	f, err := ParseWithSubst(
		"write h file $1 --data __termcp__ $2",
		Subst{Pos: []string{"/tmp/x", "value"}},
	)
	if err != nil {
		t.Fatalf("ParseWithSubst: %v", err)
	}
	st := f.Statements[0]
	if st.Key != "/tmp/x" {
		t.Errorf("key = %q, want /tmp/x — a marker in the source swallowed the fill", st.Key)
	}
	// The marker text the CALLER wrote is their bytes, and a flag value is one word:
	// `--data __termcp__ $2` hands the flag the marker and leaves $2 as a positional.
	// The point is that the marker was not read as a hole.
	d, _ := st.Flag("data")
	if d != "__termcp__" {
		t.Errorf("data = %q, want the caller's own marker text", d)
	}
}
