package mcp

import (
	"strings"
	"testing"
)

// An extension is a process, and a process holds the script it started with:
// updating that script on the target changes nothing until the session is torn down.
//
// The reconciler could already do that — the MCP tool and the admin API both reach
// it — but the DSL could not, and the DSL is the only entrance an agent gets. So an
// agent that had just updated an extension had no way to say "rebuild it", which is
// the one thing it needed to say.
func TestTermExtRestartRebuildsTheSession(t *testing.T) {
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{
		"cmd": `create ext wire --kind mcp --target internal --mode pipe --command node --args /srv/mcp.js`,
	})

	// A configuration edit answers with the stored template.
	if updated := termText(t, s, map[string]any{"cmd": `edit ext wire --description hello`}); !strings.Contains(updated, "hello") {
		t.Fatalf("edit ext should answer with the template, got %q", updated)
	}

	// --restart answers with the reconciler's acknowledgement instead: it stored
	// nothing, it rebuilt the session. If it were an update in disguise the body
	// would carry the template again.
	restarted := termText(t, s, map[string]any{"cmd": `edit ext wire --restart`})
	if !strings.Contains(restarted, "success") {
		t.Errorf("edit ext --restart should reach the retry action, got %q", restarted)
	}
}

// A rebuild and a configuration change are two operations. This refuses the
// combination by name, because the alternative — doing one and reporting success —
// leaves the caller believing it did both.
func TestTermExtRestartRefusesACombination(t *testing.T) {
	s := newExtensionTestServer(t)
	termText(t, s, map[string]any{
		"cmd": `create ext wire --kind mcp --target internal --mode pipe --command node --args /srv/mcp.js`,
	})

	body := termText(t, s, map[string]any{"cmd": `edit ext wire --restart --description hello`})
	for _, want := range []string{"--restart", "--description"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal must name %s, got %q", want, body)
		}
	}
}

// The flag is in the table the parser and the help page both read, so `help edit`
// lists it without a second list to keep in step.
func TestHelpEditListsRestart(t *testing.T) {
	s := newTestServer(t)
	if got := termText(t, s, map[string]any{"cmd": "help edit"}); !strings.Contains(got, "--restart") {
		t.Errorf("help edit does not mention --restart: %q", got)
	}
}
