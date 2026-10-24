package directive

import (
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
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
		name    string
		src     string
		sub     Subst
		want    string
		wantErr string
	}{
		{
			// Stmt.Semicolon is documented as "position of ';', '&', or '|&'", and trimming
			// it the way a ';' is trimmed dropped the backgrounding: the shell ran the
			// command in the foreground and the call blocked until it finished.
			//
			// IT IS NO LONGER THE SHELL'S `&`, and that is the point of the var payload:
			// the payload is one reference, so a `&` after it can only be this
			// language's "run concurrently". A shell-level background job goes in the
			// value — args ["sleep 100 &"] — where the shell reads it.
			name: "a backgrounding ampersand is termcp's, not the shell's",
			src:  "run s1:1 $1 &",
			sub:  Subst{Pos: []string{"sleep 100"}},
			want: "sleep 100 &",
		},
		{
			// The `;` ends the termcp statement, so it is termcp's punctuation and never the
			// shell's: `echo hi;` is not a command line the caller wrote.
			name: "a semicolon still is not",
			src:  "run s1:1 $1; ls host",
			sub:  Subst{Pos: []string{"echo hi"}},
			want: "echo hi",
		},
		{
			// ARITHMETIC AND HERE-DOCUMENTS ARE GONE WITH THE LITERAL PAYLOAD. They were
			// regions where a reference could sit inside shell syntax, and the rule is now
			// that the payload IS the reference — so the shell text (arithmetic, heredoc,
			// redirect, pipeline) travels as the VALUE, where the shell reads it whole.
			name:    "an arithmetic expansion is not a payload any more",
			src:     "run s1:1 echo $(( $1 + 1 ))",
			sub:     Subst{Pos: []string{"2"}},
			wantErr: "takes a VARIABLE",
		},
		{
			name:    "a named value in arithmetic is not either",
			src:     "run s1:1 echo $(( $N * 2 ))",
			sub:     Subst{Named: map[string]string{"N": "21"}},
			wantErr: "takes a VARIABLE",
		},
		{
			name:    "a here-document body is not a payload any more",
			src:     "run s1:1 cat <<EOF\n$1\nEOF\n",
			sub:     Subst{Pos: []string{"echo C"}},
			wantErr: "takes a VARIABLE",
		},
		{
			name:    "the tab-stripping here-document too",
			src:     "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n",
			sub:     Subst{Pos: []string{"echo C"}},
			wantErr: "takes a VARIABLE",
		},
		{
			// A QUOTED DELIMITER IS THE CALLER SAYING "EXPAND NOTHING HERE", and a payload
			// that is not a reference is refused before that question is asked.
			name:    "a quoted delimiter leaves the body alone",
			src:     "run s1:1 cat <<'EOF'\n$1\nEOF\n",
			sub:     Subst{Pos: []string{"echo C"}},
			wantErr: "never references $1",
		},
		{
			name:    "a reference beside a word in a shell line is refused",
			src:     "run s1:1 echo $1",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "cannot sit inside a shell line",
		},
		{
			name:    "an escaped dollar is not a reference",
			src:     "run s1:1 echo $" + bs + "1",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "never references $1",
		},
		{
			name:    "a single-quoted dollar is not a reference either",
			src:     "run s1:1 echo '$1'",
			sub:     Subst{Pos: []string{"a; b"}},
			wantErr: "never references $1",
		},
		{
			// A REDIRECT TARGET IS PART OF THE SHELL LINE, so a reference there is mixing
			// too — the value would be a filename, not a statement. Put the redirect inside
			// the value: cmd `run s1:1 $1`, args ["echo hi > '/tmp/a b'"].
			name:    "a reference in a redirect target is refused",
			src:     "run s1:1 echo hi > $1",
			sub:     Subst{Pos: []string{"/tmp/a b"}},
			wantErr: "cannot sit inside a shell line",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, c.sub)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseWithSubst(%q) was accepted; it mixes DSL and shell", c.src)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("ParseWithSubst(%q) failed with %v, want %q", c.src, err, c.wantErr)
				}
				return
			}
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
//
// WITH A VAR PAYLOAD THE `&` IS TERMCP'S AGAIN, and this test now pins the other half
// of that: the VALUE reaches the terminal unchanged, and a shell-level `&` is the
// caller's to put inside it.
func TestTheAmpersandReachesTheTerminal(t *testing.T) {
	f, err := ParseWithSubst("run s1:1 $1", Subst{Pos: []string{"sleep 100 &"}})
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	if !strings.HasSuffix(payload, "&") {
		t.Fatalf("payload %q lost the `&` the caller put in the value", payload)
	}

	// And outside the value it is the statement's own operator, not shell text. The
	// payload keeps it: a backgrounding `&` after a payload verb is documented as part
	// of the line the shell gets, and with the payload being one reference there is
	// nothing left for it to be confused with.
	f, err = ParseWithSubst("run s1:1 $1 &", Subst{Pos: []string{"sleep 100"}})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Statements[0].Background {
		t.Error("the statement did not become concurrent")
	}
}

// TestHeredocDelimiterQuotingComesFromTheAST pins the last byte-level syntax decision this
// pass used to make.
//
// What makes a heredoc body literal is that any part of the DELIMITER was quoted, and the
// pass used to find that out by scanning the delimiter's source slice for a quote or a
// backslash. The parser had already classified every part, so the question is a node type:
// SglQuoted or DblQuoted for a quote, a backslash inside a Lit for an escape, an ordinary
// Lit for neither. The cases below are the spellings where those differ.
func TestHeredocDelimiterQuotingComesFromTheAST(t *testing.T) {
	bs := string(rune(92))
	cases := []struct {
		name  string
		src   string
		quote bool
	}{
		{"bare delimiter", "run s1:1 cat <<EOF\n$1\nEOF\n", false},
		{"single-quoted delimiter", "run s1:1 cat <<'EOF'\n$1\nEOF\n", true},
		{"double-quoted delimiter", "run s1:1 cat <<\"EOF\"\n$1\nEOF\n", true},
		{"escaped delimiter", "run s1:1 cat <<" + bs + "EOF\n$1\nEOF\n", true},
		{"quote in the middle of the word", "run s1:1 cat <<E\"O\"F\n$1\nEOF\n", true},
		{"escape in the middle of the word", "run s1:1 cat <<E" + bs + "OF\n$1\nEOF\n", true},
		{"tab-stripping bare delimiter", "run s1:1 cat <<-EOF\n\t$1\n\tEOF\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := syntax.NewParser().Parse(strings.NewReader(c.src), "")
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.src, err)
			}
			found := false
			syntax.Walk(f, func(n syntax.Node) bool {
				r, ok := n.(*syntax.Redirect)
				if !ok || r.Hdoc == nil {
					return true
				}
				found = true
				if got := wordIsQuoted(r.Word); got != c.quote {
					t.Errorf("wordIsQuoted(%q) = %v, want %v", c.src, got, c.quote)
				}
				return true
			})
			if !found {
				t.Fatalf("%q has no heredoc to inspect", c.src)
			}
		})
	}
}
