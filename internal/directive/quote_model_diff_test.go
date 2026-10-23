package directive

import (
	"fmt"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// The substitution pass decides "is this reference inside double quotes" with a byte
// scan that TOGGLES a flag on every unescaped quote. The shell decides it with a STACK
// of quoting contexts. Those two models are not the same shape, so this is the question
// worth asking of them: do they ever disagree?
//
// The parser is the authority to disagree with — it is the shell's own grammar, and
// every `$N` it reports as a ParamExp is a reference by definition, with a quoting
// context that can be read off the nodes it sits inside. So the corpus below is run
// through both and compared reference by reference:
//
//   - a ParamExp the scan did not report is a reference the caller wrote and this
//     daemon ignored;
//   - a report with no ParamExp behind it is a reference the daemon invented;
//   - a disagreement about the quoting context is a value inserted under the wrong
//     escaping rule — `"a b"` where the shell needs `a b`, or the reverse.
//
// ONE CASE IS DELIBERATELY NOT IN THE CORPUS: a reference in the body of a heredoc whose
// DELIMITER is quoted (`<<'EOF'`). The parser is right that the shell expands nothing
// there, and this daemon is right that the caller wrote a reference it must honour — the
// two disagree because they answer different questions, and the substitution pass exists
// to answer the caller's. Leaving it out of a comparison is honest; putting it in and
// asserting either answer would pin the disagreement as if it were a rule.
//
// This is a test of the MODEL, not of a feature: it exists because "a hand-rolled
// scanner is obviously worse than the parser" is an assumption, and assumptions about
// the trickiest code in this package are how issue #141 happened.
func TestTheScanAndTheParserAgreeOnEveryReference(t *testing.T) {
	corpus := []string{
		`echo $1`,
		`echo "$1"`,
		`echo '$1'`,
		`echo a$1b`,
		`echo "$1" '$2' $3`,
		`echo "${1}"`,
		`echo "\${1}"`,
		`echo "\"$1\""`,
		`echo "$(echo "x") $1"`,
		`echo "$(echo 'x') $1"`,
		`echo "$(echo $1) $2"`,
		`echo $(echo "$1") $2`,
		`echo "` + "`echo $1`" + ` $2"`,
		`echo $(( $1 + 1 ))`,
		`echo "$(( $1 + 1 ))"`,
		`echo "a
$1"`,
		`echo "a\"b $1"`,
		`echo 'a"b' $1`,
		`echo $'$1' $2`,
		`echo $"\$1" $2`,
		`echo "x${1}y"`,
		`echo "x$1y"`,
		`echo "" $1 ""`,
		`echo ""$1""`,
		`cat <<EOF
$1
EOF`,
		`echo "$1" | cat`,
		`echo a "$1 b" c`,
	}

	for _, src := range corpus {
		src := src
		t.Run(fmt.Sprintf("%q", src), func(t *testing.T) {
			want := parserRefs(t, src)
			got := scanRefs(t, src)
			if len(want) != len(got) {
				t.Fatalf("the scan reported %d reference(s), the parser reports %d\n scan: %v\nparse: %v",
					len(got), len(want), got, want)
			}
			for i := range want {
				if got[i].start != want[i].start {
					t.Errorf("reference %d: the scan points at %d (%q), the parser at %d (%q)",
						i, got[i].start, src[got[i].start:got[i].end], want[i].start, src[want[i].start:want[i].end])
					continue
				}
				if got[i].dq != want[i].dq {
					t.Errorf("reference %q: the scan says inDoubleQuotes=%v, the parser says %v",
						src[got[i].start:got[i].end], got[i].dq, want[i].dq)
				}
			}
		})
	}
}

type ref struct {
	start, end int
	dq         bool
}

// parserRefs lists every parameter expansion the PARSER sees, with the quoting context
// read off the nodes it sits inside.
//
// A single-quoted string is a leaf in this grammar (its text is one field), so a `$1`
// inside one has no node at all and cannot appear here — which is exactly the property
// the substitution pass has to reproduce rather than assume.
func parserRefs(t *testing.T, src string) []ref {
	t.Helper()
	f, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		t.Fatalf("the corpus entry does not parse: %v", err)
	}
	var dq [][2]int
	syntax.Walk(f, func(n syntax.Node) bool {
		if q, ok := n.(*syntax.DblQuoted); ok {
			dq = append(dq, [2]int{int(q.Pos().Offset()), int(q.End().Offset())})
		}
		return true
	})
	var out []ref
	syntax.Walk(f, func(n syntax.Node) bool {
		p, ok := n.(*syntax.ParamExp)
		if !ok {
			return true
		}
		s, e := int(p.Pos().Offset()), int(p.End().Offset())
		r := ref{start: s, end: e}
		for _, q := range dq {
			if s >= q[0] && e <= q[1] {
				r.dq = true
			}
		}
		out = append(out, r)
		return true
	})
	return out
}

// scanRefs lists what the substitution pass finds, asked with a table that defines
// everything so that every reference it RECOGNISES is reported rather than skipped.
func scanRefs(t *testing.T, src string) []ref {
	t.Helper()
	sub := Subst{
		Named: map[string]string{"ANY": "v"},
		Pos:   []string{"1", "2", "3", "4", "5", "6", "7", "8", "9"},
	}
	occs, err := scanSubstitutions(src, sub, payloadSpans(src))
	if err != nil {
		// A reference the scan refuses (an out-of-range index, a $0) is still a
		// reference it recognised; the corpus has none, so this is a real surprise.
		t.Fatalf("the scan refused the corpus entry: %v", err)
	}
	var out []ref
	for _, o := range occs {
		out = append(out, ref{start: o.start, end: o.end, dq: o.dq})
	}
	return out
}
