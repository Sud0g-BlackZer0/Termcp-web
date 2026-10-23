package directive

import (
	"strings"
	"testing"
)

// These pin the regions the parser knows about, after the pass stopped deciding them on
// its own.
//
// The rule they all follow is one sentence: a reference is a `$` the grammar reported, and
// what the grammar did not report the caller deliberately kept from being parsed. Each case
// below is that sentence landing somewhere a hand-rolled rule used to answer differently —
// a `&` read as a separator, an arithmetic expression read as a word list, a heredoc body
// read as a line, a flag value read as shell.
func TestRegionsTheParserAlreadyKnows(t *testing.T) {
	bs := string(rune(92))
	cases := []struct {
		name string
		src  string
		sub  Subst
		want string
	}{
		{
			// Stmt.Semicolon is documented as "position of ';', '&', or '|&'", and trimming
			// it the way a ';' is trimmed dropped the backgrounding: the shell ran the
			// command in the foreground and the call blocked until it finished.
			name: "a backgrounding ampersand is part of the line",
			src:  "run s1:1 sleep 100 &",
			want: "sleep 100 &",
		},
		{
			// The `;` ends the termcp statement, so it is termcp's punctuation and never the
			// shell's: `echo hi;` is not a command line the caller wrote.
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
			// Redirect.Hdoc IS the body, terminator excluded — the parser read it, so its
			// bounds are not a scan for a line that looks like the terminator.
			name: "a here-document body is text, not a word",
			src:  "run s1:1 cat <<EOF\n$1\nEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<EOF\necho C\nEOF",
		},
		{
			// A QUOTED DELIMITER IS THE CALLER SAYING "EXPAND NOTHING HERE", and the grammar
			// answers accordingly: no nodes inside the body, so no references, so the text
			// arrives as written. The same rule as everywhere else in this pass rather than a
			// heredoc special case — which is what makes it predictable.
			name: "a quoted delimiter leaves the body alone",
			src:  "run s1:1 cat <<'EOF'\n$1\nEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<'EOF'\n$1\nEOF",
		},
		{
			name: "the tab-stripping form too",
			src:  "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n",
			sub:  Subst{Pos: []string{"echo C"}},
			want: "cat <<-EOF\n\techo C\n\tEOF",
		},
		{
			// The narrowing that must not be lost: a reference beside a word is one quoted
			// word, and a `$` the caller kept from the parser is the caller's literal.
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
		{
			// A redirect target is inside the command line but outside the payload words, so
			// it is shell (quoted as data) rather than a flag value (bare). One number used to
			// answer both questions, and `> $1` arrived bare in a redirect — a syntax error
			// for any value with a space in it.
			name: "a redirect target is shell, and quoted as data",
			src:  "run s1:1 echo hi > $1",
			sub:  Subst{Pos: []string{"/tmp/a b"}},
			want: "echo hi > '/tmp/a b'",
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

// TestTheAmpersandReachesTheTerminal is the end-to-end half of the first case above: the
// payload is what gets typed into the shell, so a dropped `&` is a foreground command
// however clean the parse looked.
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
