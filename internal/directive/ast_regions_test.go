package directive

import (
	"strings"
	"testing"
)

// These pin the regions the PARSER knows about and the byte scan used to guess at.
//
// The substitution pass is a scan, and a scan is the right shape for it: a verbatim
// region (a JSON frame, a target) has no shell to honour, so the shell's own quoting
// is not a rule there, and the parser cannot see inside a single-quoted string to
// find a `$1` that a frame directive's help tells the caller to write. What the scan
// must not do is decide things the node types already decide, and each case below was
// exactly that: a region with a node of its own — a `&`, an arithmetic expansion, a
// here-document body — was read as plain bytes, and the plain-byte reading was wrong.
func TestRegionsTheParserAlreadyKnows(t *testing.T) {
	bs := string(rune(92))
	cases := []struct {
		name string
		src  string
		sub  Subst
		want string
	}{
		{
			// Stmt.Semicolon is documented as "position of ';', '&', or '|&'", and
			// trimming it the way a ';' is trimmed dropped the backgrounding: the shell
			// ran `sleep 100` in the foreground and the call blocked on it.
			name: "a backgrounding ampersand is part of the line",
			src:  "run s1:1 sleep 100 &",
			want: "sleep 100 &",
		},
		{
			// The `;` ends the termcp statement, so it is termcp's punctuation and the
			// payload never carries it: `echo hi;` is not what the caller wrote a
			// command line of.
			name: "a semicolon still is not",
			src:  "run s1:1 echo hi; ls host",
			want: "echo hi",
		},
		{
			// A quoted word is not an operand. The parser reports the region as an
			// ArithmExp, so nothing has to guess that `$((` opened an expression.
			name: "an arithmetic expansion takes a bare operand",
			src:  "run s1:1 echo $(( $1 + 1 ))",
			sub:  Subst{Pos: []string{"2"}},
			want: "echo $(( 2 + 1 ))",
		},
		{
			name: "a named value in arithmetic is bare as well",
			src:  "run s1:1 echo $(( $N * 2 ))",
			sub:  Subst{Named: map[string]string{"N": "21"}},
			want: "echo $(( 21 * 2 ))",
		},
		{
			// Redirect.Hdoc IS the body, terminator excluded — the parser read it, so
			// its bounds are not a scan for a line that looks like the terminator.
			name: "a here-document body is text, not a word",
			src:  "run s1:1 cat <<EOF\n$1\nEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<EOF\necho C\nEOF",
		},
		{
			name: "a quoted delimiter does not change that",
			src:  "run s1:1 cat <<'EOF'\n$1\nEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<'EOF'\necho C\nEOF",
		},
		{
			name: "the tab-stripping form too",
			src:  "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<-EOF\n\techo C\n\tEOF",
		},
		{
			// The narrowing that must not be lost: a reference beside a word is still
			// one quoted word, and a `$` the caller escaped is still not a reference.
			name: "a reference beside a word is still quoted",
			src:  "run s1:1 echo $1",
			sub:  Subst{Pos: []string{"a; b"}},
			want: "echo 'a; b'",
		},
		{
			name: "an escaped dollar is not a reference",
			src:  "run s1:1 echo $" + bs + "1",
			sub:  Subst{Pos: []string{"a; b"}},
			want: "echo $" + bs + "1",
		},
		{
			name: "a single-quoted dollar is not a reference either",
			src:  "run s1:1 echo '$1'",
			sub:  Subst{Pos: []string{"a; b"}},
			want: "echo '$1'",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, c.sub)
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", c.src, err)
			}
			if len(f.Statements) == 0 {
				t.Fatalf("%q parsed into no statements", c.src)
			}
			if got := f.Statements[0].Payload; got != c.want {
				t.Errorf("payload of %q:\n got: %q\nwant: %q", c.src, got, c.want)
			}
		})
	}
}

// TestTheAmpersandReachesTheTerminal is the end-to-end half of the first case above:
// the payload is what gets typed into the shell, so a dropped `&` is a foreground
// command however clean the parse looked.
func TestTheAmpersandReachesTheTerminal(t *testing.T) {
	f, err := ParseWithSubst("run s1:1 sleep 100 &", Subst{})
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	if !strings.HasSuffix(payload, "&") {
		t.Fatalf("payload %q does not background the command; the caller asked for `&`", payload)
	}
}
