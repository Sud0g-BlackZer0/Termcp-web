package mcp

import (
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestNamedKindNeverFallsThroughToTheContainer pins the difference between "close
// this peer" and "close this session".
//
// `close <session> mcp 1` used to parse the kind away, find no branch for it, and
// take the default path — closing the SESSION and every channel on it. A caller
// naming a peer destroyed the whole link and got a clean 204 for it. The same shape
// as #132: a qualifier treated as decoration instead of as the object.
func TestNamedKindNeverFallsThroughToTheContainer(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]

	// A kind this verb does not handle must be refused, not silently downgraded.
	for _, cmd := range []string{"close " + sid + " mcp 1", "close " + sid + " file /tmp/x"} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !strings.HasPrefix(got, "TERM/1 4") {
			t.Errorf("%q was accepted; a named kind must never fall through:\n%s", cmd, got)
		}
	}

	// And the session is still there, which is the whole point: the refusals above
	// must not have closed anything.
	after := termText(t, s, map[string]any{"cmd": "ls"})
	if !strings.Contains(after, sid) {
		t.Fatalf("the session was closed by a refused command:\n%s", after)
	}
	termText(t, s, map[string]any{"cmd": "close " + h})
}

// TestADeclaredKindIsEitherHandledOrRefused is the general form of the test above:
// for every verb and every kind it declares, `verb <session> <kind> <key>` must
// either reach a branch for that kind or be refused outright. What it must never do
// is answer 2xx from a path that ignored the kind, because that is a command that
// did something other than what was asked.
//
// The session is a live pipe and the key is deliberately nonsense, so a branch that
// does run will fail on its own terms — a 4xx here means "the kind was not silently
// discarded", which is exactly the property under test.
func TestADeclaredKindIsEitherHandledOrRefused(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, op := range directive.Ops() {
		for _, kind := range op.Kinds {
			cmd := op.Name + " " + sid + " " + kind + " zz-nonsense"
			res := termCall(t, s, map[string]any{"cmd": cmd})
			body := toolResultText(res)
			// A 2xx is FINE when the branch really handled the kind: `ls forward` on a
			// session with no tunnels is an empty list, and `rm alias nosuch` is a no-op
			// that succeeds. What is not fine is a 2xx that ignored the kind and acted on
			// the session — and that failure shows up as the session disappearing, which
			// the check below catches.
			if res.IsError && strings.Contains(body, "does not take a") {
				continue // refused by name, which is the other acceptable answer
			}
			if !res.IsError && !strings.Contains(body, "TERM/1 2") {
				t.Errorf("%q answered neither a refusal nor a result:\n%s", cmd, body)
			}
		}
	}

	// The session survived every one of them, which also proves none of the refusals
	// took the default "act on the target" path.
	if after := termText(t, s, map[string]any{"cmd": "ls"}); !strings.Contains(after, sid) {
		t.Fatalf("a declared-kind probe closed the session:\n%s", after)
	}
}

// TestEveryFileOpReachesItsGroup pins the two-level SFTP surface from the caller's
// side: `--op chmod` must reach the perm group, `--op realpath` the fs group, and so
// on. The group is an implementation detail; a caller names the operation.
//
// It exists because the first version of `get` hardcoded the fs group, so `--op stat`
// answered "action must be truncate, realpath, or statvfs" — the operation was real,
// the group was wrong, and the message described a layer the caller never mentioned.
func TestEveryFileOpReachesItsGroup(t *testing.T) {
	s := newTestServer(t)
	h := openPipe(t, s, "sleep 30")
	sid := strings.SplitN(h, ":", 2)[0]
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for _, op := range []string{
		"stat", "getwd", "urls", "realpath", "readlink",
		"chmod", "chown", "chtimes", "symlink", "link", "truncate", "statvfs",
	} {
		for _, verb := range []string{"get", "write"} {
			cmd := verb + " " + sid + " file /tmp/x --op " + op
			got := termText(t, s, map[string]any{"cmd": cmd})
			// "does not know" means the table is missing the op; any other answer —
			// success, or a failure about the path — means the group was found.
			if strings.Contains(got, "does not know") {
				t.Errorf("%q: --op %s is not mapped to a group:\n%s", cmd, op, got)
			}
			// And the group's own complaint must never be what the caller sees.
			if strings.Contains(got, "action must be truncate, realpath, or statvfs") {
				t.Errorf("%q: reached the fs group with an op it does not hold:\n%s", cmd, got)
			}
		}
	}
}

// TestEveryKindTheDispatcherServesIsParsable is the REVERSE of
// TestADeclaredKindIsEitherHandledOrRefused, and it is the one that would have
// caught `create <session> file <path>`.
//
// That test starts from the declared kinds and asks whether each is handled. It
// cannot see a branch that exists in the dispatcher but whose kind was never
// declared — and `create`'s file branch was exactly that: unreachable, because the
// parser had no reason to read `file` as a kind and left it in Args. The verb
// answered "create needs a kind" for a shape its own usage line advertised.
//
// So this walks the OTHER direction: for every verb-and-kind pair the dispatcher
// serves, the statement must parse into that kind and key. A new branch with no
// declaration fails here.
func TestEveryKindTheDispatcherServesIsParsable(t *testing.T) {
	cases := []struct{ cmd, wantKind, wantKey string }{
		// open
		{"open s1 forward --local-port 0", "forward", ""},
		{"open s1 mcp http://h/mcp", "mcp", "http://h/mcp"},
		{"open s1 cdp ws://h/d", "cdp", "ws://h/d"},
		{"open s1 jrpc ws://h/s", "jrpc", "ws://h/s"},
		// ls
		{"ls host", "host", ""},
		{"ls ext", "ext", ""},
		{"ls alias", "alias", ""},
		{"ls s1 forward", "forward", ""},
		{"ls s1 file /tmp", "file", "/tmp"},
		// rm
		{"rm s1 file /tmp/x", "file", "/tmp/x"},
		{"rm host prod", "host", "prod"},
		{"rm ext wire", "ext", "wire"},
		{"rm alias ll", "alias", "ll"},
		// close
		{"close s1 forward fw-1", "forward", "fw-1"},
		// read / write / get
		{"read s1 file /tmp/x", "file", "/tmp/x"},
		{"write s1 file /tmp/x --data y", "file", "/tmp/x"},
		{"get s1 file /tmp/x", "file", "/tmp/x"},
		// create / edit
		{"create host prod", "host", "prod"},
		{"create ext wire", "ext", "wire"},
		{"create alias ll ls", "alias", "ll"},
		{"create s1 file /tmp/new", "file", "/tmp/new"},
		{"edit host prod", "host", "prod"},
		{"edit ext wire", "ext", "wire"},
		{"edit alias ll", "alias", "ll"},
	}
	for _, tc := range cases {
		f, err := directive.Parse(tc.cmd)
		if err != nil {
			t.Errorf("%q does not parse: %v", tc.cmd, err)
			continue
		}
		st := f.Statements[0]
		if st.Kind != tc.wantKind {
			t.Errorf("%q parsed kind = %q, want %q (args=%v)", tc.cmd, st.Kind, tc.wantKind, st.Args)
		}
		if st.Key != tc.wantKey {
			t.Errorf("%q parsed key = %q, want %q", tc.cmd, st.Key, tc.wantKey)
		}
	}
}
