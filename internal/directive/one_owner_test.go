package directive

import (
	"strings"
	"testing"
)

// THE INVARIANT THIS FILE PINS: every reference has exactly ONE owner, and WHICH owner
// depends on the region rather than on a rule invented per case.
//
// The substitution pass used to decide, byte by byte, whether a `$1` counted — with a
// quote-toggle, a span table, and a special case per region. Each of those decisions was a
// second opinion about something the grammar already states, and each second opinion was a
// place to be wrong: a `&` trimmed off a command line, a `$(( ))` operand quoted, a heredoc
// body quoted, a `--data '$1'` silently not substituted.
//
// The model that removes the class is smaller, not bigger:
//
//	In a SHELL region — a payload command line, an argv list — the grammar owns it: the
//	references are the parser's nodes, and a `$` the parser did not report is not one.
//
//	In EVERY OTHER region — a frame, a target, a flag value, a heredoc body — the BYTES
//	own it, because no shell will ever read them: a quote there is a byte rather than
//	syntax, so `--data '$1'` and `call h '{"e":"$1"}'` are references.
//
// The two halves are stated as outcomes below rather than derived from the parser, because
// a test whose oracle is the implementation proves nothing.

// TestEveryRegionStatesHowItTakesAValue pins the insertion half: once a reference is known,
// the region decides how the value goes in, and there are only a handful of answers.
//
// This part must NOT come from the parser — the parser does not know what a destination
// will do with the bytes — so it is a table, small enough to read.
func TestEveryRegionStatesHowItTakesAValue(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sub  Subst
		want string
		flag string // when set, the assertion is about this flag's value instead
	}{
		{"a command line quotes it", `run s1:1 echo $1`, Subst{Pos: []string{"a b"}}, `echo 'a b'`, ""},
		{"the whole payload is the caller's line", `run s1:1 $1`, Subst{Pos: []string{"a b"}}, `a b`, ""},
		{"a frame is verbatim", `call s1:1 cdp '{"e":"$1"}'`, Subst{Pos: []string{"2+3"}}, `'{"e":"2+3"}'`, ""},
		{"an arithmetic expression is bare", `run s1:1 echo $(( $1 + 1 ))`, Subst{Pos: []string{"2"}}, `echo $(( 2 + 1 ))`, ""},
		{"a heredoc body is text", "run s1:1 cat <<EOF\n$1\nEOF\n", Subst{Pos: []string{"echo C"}}, "cat <<EOF\necho C\nEOF", ""},
		// A FLAG VALUE IS SUBSTITUTED WITH THE PARSER'S RECOGNITION AND INSERTED BARE. The
		// two halves come from different places and both are stated here: whether a `$1`
		// counts is the grammar's answer (so `'$1'` is the caller's literal), and how the
		// value goes in is the destination's (a flag value is not shell, so nothing quotes
		// it — `--data "a b"` and `--data $1` both arrive as `a b`).
		{"a flag value is inserted bare", `write s1 file /tmp/x --data "$1"`, Subst{Pos: []string{"a b"}}, "", "data"},
		{"and unquoted too", `write s1 file /tmp/x --data $1`, Subst{Pos: []string{"a b"}}, "", "data"},
		{"but a single-quoted one is the caller's literal", `write s1 file /tmp/x --data '$1'`, Subst{Pos: []string{"a b"}}, "", "data-literal"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, c.sub)
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", c.src, err)
			}
			st := f.Statements[0]
			if c.flag != "" {
				want := "a b"
				if c.flag == "data-literal" {
					want = "$1"
				}
				if got, _ := st.Flag("data"); got != want {
					t.Errorf("--data with args [%q] arrived as %q, want %q", c.sub.Pos[0], got, want)
				}
				return
			}
			if st.Payload != c.want {
				t.Errorf("payload of %q:\n got: %q\nwant: %q", c.src, st.Payload, c.want)
			}
		})
	}
}

// TestWhatCountsAsAReference pins the recognition half, including the rows where a caller
// could reasonably expect the opposite answer.
func TestWhatCountsAsAReference(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sub  Subst
		want string
		// err is the text the refusal must contain, when the line is refused.
		err string
	}{
		{name: "a positional reference is substituted", src: `run s1:1 echo $1`,
			sub: Subst{Pos: []string{"a b"}}, want: `echo 'a b'`},
		{name: "a braced one too", src: `run s1:1 echo ${1}`,
			sub: Subst{Pos: []string{"a b"}}, want: `echo 'a b'`},
		{name: "a named one", src: `run s1:1 echo $N`,
			sub: Subst{Named: map[string]string{"N": "a b"}}, want: `echo 'a b'`},
		{name: "a reference glued to a word is still one", src: `run s1:1 echo x$N`,
			sub: Subst{Named: map[string]string{"N": "a"}}, want: `echo x'a'`},

		// THE SHELL'S OWN CONSTRUCTS ARE NOT REFERENCES. Rewriting a construct whose
		// meaning depends on a variable this daemon never sees would be a guess.
		{name: "the positional list is the shell's", src: `run s1:1 echo $@`,
			sub: Subst{Pos: []string{"a"}}, want: `echo $@`},
		{name: "a default value is the shell's", src: `run s1:1 echo ${N:-x}`,
			sub: Subst{Named: map[string]string{"N": "a"}}, want: `echo ${N:-x}`},
		{name: "a length is the shell's", src: `run s1:1 echo ${#N}`,
			sub: Subst{Named: map[string]string{"N": "a"}}, want: `echo ${#N}`},
		{name: "a command substitution is the shell's", src: `run s1:1 echo $(date)`,
			sub: Subst{Named: map[string]string{"N": "a"}}, want: `echo $(date)`},
		{name: "an undefined name is left for the shell when vars is empty", src: `run s1:1 echo $N`,
			want: `echo $N`},
		{name: "and named as a mistake when vars is in use", src: `run s1:1 echo $NOPE`,
			sub: Subst{Named: map[string]string{"N": "a"}}, err: "vars does not define $NOPE"},
		{name: "an out-of-range positional is refused", src: `run s1:1 echo $3`,
			sub: Subst{Pos: []string{"a"}}, err: "args has 1 element"},
		{name: "$0 has no argument to name", src: `run s1:1 echo $0`,
			sub: Subst{Pos: []string{"a"}}, err: "args is 1-based"},

		// A SINGLE-QUOTED DOLLAR IN A SHELL REGION IS THE SHELL'S LITERAL, and that is the
		// answer rather than a special case: the parser reports no node there, so there is no
		// reference, and no rule had to be written for it. Substituting it would mean
		// rebuilding the quoting around a value that is arbitrary text; passing it through
		// unchanged is what the grammar already said.
		{name: "a quoted dollar in a command line is the shell's", src: `run s1:1 echo '$1'`,
			sub: Subst{Pos: []string{"a"}}, want: `echo '$1'`},
		{name: "and a quoted name too", src: `run s1:1 echo '$N'`,
			sub: Subst{Named: map[string]string{"N": "a"}}, want: `echo '$N'`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := ParseWithSubst(c.src, c.sub)
			if c.err != "" {
				if err == nil {
					t.Fatalf("%q was accepted; it must be refused", c.src)
				}
				if !strings.Contains(err.Error(), c.err) {
					t.Fatalf("the refusal does not say %q: %v", c.err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseWithSubst(%q): %v", c.src, err)
			}
			if got := f.Statements[0].Payload; got != c.want {
				t.Errorf("payload of %q:\n got: %q\nwant: %q", c.src, got, c.want)
			}
		})
	}
}
