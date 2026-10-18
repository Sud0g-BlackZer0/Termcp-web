package directive

import (
	"strings"
	"testing"
)

// TestPayloadIsSlicedNotReparsed pins the rule the whole format depends on: the
// payload is a SLICE OF THE SOURCE, so quoting, escaping and `$()` reach the shell
// exactly as the caller wrote them.
//
// The case that makes it a hard requirement is `'a'"b"'c'`: three shell parts in
// one word. Re-joining the parsed words yields `abc`, which is a different command
// line. Slicing yields the original.
func TestPayloadIsSlicedNotReparsed(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "a pipeline with quotes and && survives byte for byte",
			cmd:  `run a1f3:1 ls -la | grep -c '\.go$' && echo "rc=$?"`,
			want: `ls -la | grep -c '\.go$' && echo "rc=$?"`,
		},
		{
			name: "adjacent quoted parts are not merged",
			cmd:  `run a1f3:1 'a'"b"'c'`,
			want: `'a'"b"'c'`,
		},
		{
			name: "an unquoted multi-word payload is not re-quoted",
			cmd:  `run a1f3:1 echo a b`,
			want: `echo a b`,
		},
		{
			name: "dollar constructs are passed through, not expanded",
			cmd:  `run a1f3:1 'echo $HOME'`,
			want: `'echo $HOME'`,
		},
		{
			name: "backslashes survive",
			cmd:  `run a1f3:1 echo a\ b`,
			want: `echo a\ b`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse(tc.cmd)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if len(f.Statements) != 1 {
				t.Fatalf("statements = %d, want 1", len(f.Statements))
			}
			if got := f.Statements[0].Payload; got != tc.want {
				t.Errorf("payload = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyParseNeverEvaluate pins that a payload is not interpreted: the parser
// does not expand $HOME, $(), or backticks, because expansion is the shell's job
// and doing it here would break the zero-rewrite guarantee.
func TestOnlyParseNeverEvaluate(t *testing.T) {
	f, err := Parse(`run a1f3:1 'echo $HOME $(date) ` + "`id`" + `'`)
	if err != nil {
		t.Fatal(err)
	}
	payload := f.Statements[0].Payload
	for _, want := range []string{"$HOME", "$(date)", "`id`"} {
		if !strings.Contains(payload, want) {
			t.Errorf("payload %q lost %q: the parser must not evaluate", payload, want)
		}
	}
}

// TestHashIsNotATargetPrefix pins the correction that produced the current
// syntax. `#` starts a comment in the shell grammar, so `run #a1f3:1 'x'` parses
// to a single word. That must be an ERROR with a usage line, never a silent
// no-op or an attempt to "fix" it.
func TestHashIsNotATargetPrefix(t *testing.T) {
	_, err := Parse(`run #a1f3:1 'x'`)
	if err == nil {
		t.Fatal("`run #a1f3:1` must fail: '#' is a comment, so the target is gone")
	}
	var derr *Error
	if !asError(err, &derr) {
		t.Fatalf("error is %T, want *directive.Error", err)
	}
	if !strings.Contains(derr.Msg, "needs a target") {
		t.Errorf("message = %q, want it to say the target is missing", derr.Msg)
	}
	if derr.Usage == "" {
		t.Error("the failure must carry a usage line, so a caller learns the shape")
	}
}

// TestLocatorSpellings pins both accepted target spellings: the short form
// (which every reply emits) and the old termcp:// form (which the Web UI's copy
// button still produces). The `#` inside a word is not a comment.
func TestLocatorSpellings(t *testing.T) {
	for _, cmd := range []string{`run a1f3:1 'x'`, `run termcp://#a1f3:1 'x'`} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if got := f.Statements[0].Target; got != strings.Fields(cmd)[1] {
			t.Errorf("%s: target = %q", cmd, got)
		}
	}
}

// TestBatchTargetIsOneWord pins that a comma-separated target is a single word:
// the shell does not split on commas, so a batch needs no new syntax.
func TestBatchTargetIsOneWord(t *testing.T) {
	f, err := Parse(`run a1f3:1,b7c2:1 'x'`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Target; got != "a1f3:1,b7c2:1" {
		t.Errorf("target = %q, want the whole comma list as one word", got)
	}
}

// TestFlagsComeAfterTheTarget pins the positional rule: word 2 is the target, so
// a flag in second position is refused rather than read as a target.
func TestFlagsComeAfterTheTarget(t *testing.T) {
	f, err := Parse(`key a1f3:1 up --repeat 3`)
	if err != nil {
		t.Fatal(err)
	}
	st := f.Statements[0]
	if st.Target != "a1f3:1" {
		t.Errorf("target = %q", st.Target)
	}
	if len(st.Args) != 1 || st.Args[0] != "up" {
		t.Errorf("args = %v, want [up]", st.Args)
	}
	if st.Flags["repeat"] != "3" {
		t.Errorf("repeat = %q, want 3", st.Flags["repeat"])
	}

	if _, err := Parse(`key --repeat 3 a1f3:1 up`); err == nil {
		t.Fatal("a flag before the target must be refused: it would silently become the target")
	} else if !strings.Contains(err.Error(), "after the target") {
		t.Errorf("error = %v, want it to say flags come after the target", err)
	}
}

// TestFlagWithValue pins both spellings of a valued flag.
func TestFlagWithValue(t *testing.T) {
	f, err := Parse(`read a1f3:1 --cursor 1024 --stream o --raw`)
	if err != nil {
		t.Fatal(err)
	}
	flags := f.Statements[0].Flags
	if flags["cursor"] != "1024" {
		t.Errorf("cursor = %q", flags["cursor"])
	}
	if _, ok := flags["raw"]; !ok {
		t.Error("a valueless flag must still be present: 'was it set' and 'what to' are two questions")
	}
	if flags["stream"] != "o" {
		t.Errorf("stream = %q", flags["stream"])
	}

	f, err = Parse(`read a1f3:1 --cursor=2048`)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Flags["cursor"]; got != "2048" {
		t.Errorf("--cursor=2048 parsed as %q", got)
	}
}

// TestUnknownDirectiveAndFlag pins the two failures learn_mode turns into a
// Usage line: a name that is not a directive, and a flag that is not that
// directive's.
func TestUnknownDirectiveAndFlag(t *testing.T) {
	if _, err := Parse(`raed a1f3:1 'x'`); err == nil {
		t.Fatal("an unknown directive must fail")
	} else if !strings.Contains(err.Error(), "unknown directive") {
		t.Errorf("error = %v", err)
	}

	if _, err := Parse(`read a1f3:1 --bogus 1`); err == nil {
		t.Fatal("an unknown flag must fail")
	} else if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("error = %v", err)
	}
}

// TestStatementsSplitOnSemicolonNotOnAnd pins the sequencing rule: `;` asks two
// questions and gets two slots, while `&&` stays inside one payload because it is
// one command whose result the caller wants.
func TestStatementsSplitOnSemicolonNotOnAnd(t *testing.T) {
	f, err := Parse(`run a1f3:1 'x'; run b7c2:1 'y'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 2 {
		t.Fatalf("statements = %d, want 2", len(f.Statements))
	}
	if f.Statements[0].Target != "a1f3:1" || f.Statements[1].Target != "b7c2:1" {
		t.Errorf("targets = %q, %q", f.Statements[0].Target, f.Statements[1].Target)
	}

	f, err = Parse(`run a1f3:1 make && echo ok`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Statements) != 1 {
		t.Fatalf("`&&` split into %d statements; it is one command line", len(f.Statements))
	}
	if got := f.Statements[0].Payload; got != "make && echo ok" {
		t.Errorf("payload = %q", got)
	}
}

// TestEmptyIsLegal pins that an empty cmd is the "where am I" call: legal and
// cheap, never an error.
func TestEmptyIsLegal(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\n"} {
		f, err := Parse(cmd)
		if err != nil {
			t.Fatalf("%q: %v", cmd, err)
		}
		if !f.Empty {
			t.Errorf("%q parsed as non-empty", cmd)
		}
	}
}

// TestPathIsTheCommandEcho pins the Path header: the caller's own bytes for that
// statement, so a combined command can be reconciled against what came back.
func TestPathIsTheCommandEcho(t *testing.T) {
	cmd := `run a1f3:1 'test -f go.mod && go test ./... || echo no-module'`
	f, err := Parse(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Statements[0].Path; got != cmd {
		t.Errorf("Path = %q, want %q", got, cmd)
	}
}

// TestEveryDirectiveParsesItsOwnUsage keeps the table honest: the usage line a
// caller is shown must be a line this parser accepts, once its placeholders are
// filled. A usage that does not parse teaches the wrong shape — which is exactly
// what a `run #handle 'cmd'` example would do, since '#' is a comment.
func TestEveryDirectiveParsesItsOwnUsage(t *testing.T) {
	// Longest first: "<handle|session>" must be replaced before "<session>".
	placeholders := []string{
		"<entry|session>", "<session|handle>", "<handle|session>",
		"<handle>", "<session>", "<key>", "<rows>", "<cols>", "<body>",
		"<directive>", "<name>",
	}
	for _, op := range Ops() {
		// The usage lines are documentation: they show alternatives with `|` and
		// optional parts in brackets. Take the first alternative, drop the
		// optional parts, and fill the placeholders — enough to prove that the
		// directive name and the flag names are real and in a legal position.
		first := strings.Split(op.Usage, "|")[0]
		var line strings.Builder
		for _, w := range strings.Fields(first) {
			if strings.HasPrefix(w, "[") || strings.HasPrefix(w, "...") {
				continue
			}
			line.WriteString(" ")
			line.WriteString(w)
		}
		got := line.String()
		for _, p := range placeholders {
			got = strings.ReplaceAll(got, p, "x")
		}
		got = strings.TrimSpace(got)
		if strings.Contains(got, "<") || strings.Contains(got, "…") || strings.HasSuffix(got, ":") {
			continue // the first alternative is prose, not a template
		}
		if _, err := Parse(got); err != nil {
			t.Errorf("%s: its own usage %q does not parse: %v", op.Name, got, err)
		}
	}
}

// asError is errors.As without the import, kept local so the test reads plainly.
func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}
