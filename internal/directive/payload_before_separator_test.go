package directive

import "testing"

// TestAPayloadBeforeASeparatorIsStillTheCommandLine pins issues #141 and #155.
//
// A positional IS the payload only when it is the whole span, and the span was a BYTE
// RANGE: one trailing space was enough to answer no. Three forms a caller writes
// constantly were answered no for exactly that reason, and each turned a command line
// into a single quoted word:
//
//	run h:1 $1 &               span "$1 &"   terminal got `'echo C' &`
//	run h:1 $1 ; ls host       span "$1 "    terminal got `'echo D' `
//	run h:1 $1 && run h:1 $2   (one shell statement, so `$2` had no span at all)
//
// In all three the caller DID name the command line: what follows is the punctuation
// that ends or backgrounds it, not another word of it. The reply said 200 in every
// case, and the terminal answered `command not found: echo C` — the value arrived, and
// the command had silently stopped being a command line.
func TestAPayloadBeforeASeparatorIsStillTheCommandLine(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		args []string
		// want is the payload of each statement, in order.
		want []string
	}{
		{"background", `run h:1 $1 &`, []string{"echo C"}, []string{"echo C "}},
		{"separator", `run h:1 $1 ; ls host`, []string{"echo D"}, []string{"echo D ", ""}},
		{"and chain", `run h:1 $1 && run h:1 $2`, []string{"echo A", "echo B"}, []string{"echo A", "echo B"}},
		{"or chain", `run h:1 $1 || run h:1 $2`, []string{"echo A", "echo B"}, []string{"echo A", "echo B"}},
		// The narrowing that must survive: a reference with words on EITHER side is a
		// word in a line the caller wrote, and quoting is right there.
		{"word before it", `run h:1 echo $1`, []string{"a; b"}, []string{"echo 'a; b'"}},
		{"pipeline head", `run h:1 $1 | wc -l`, []string{"echo C"}, []string{"'echo C' | wc -l"}},
		{"redirect", `run h:1 $1 > /tmp/out`, []string{"echo C"}, []string{"'echo C' > /tmp/out"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseWithSubst(tc.cmd, Subst{Pos: tc.args})
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", tc.cmd, err)
			}
			if len(f.Statements) != len(tc.want) {
				t.Fatalf("%q parsed into %d statements, want %d", tc.cmd, len(f.Statements), len(tc.want))
			}
			for i, want := range tc.want {
				if got := f.Statements[i].Payload; got != want {
					t.Errorf("statement %d payload = %q, want %q", i, got, want)
				}
			}
		})
	}
}
