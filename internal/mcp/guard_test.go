package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// guardTestSession creates a running session carrying the given restriction, so
// a guard test exercises a real session the way a restricted extension would.
func guardTestSession(t *testing.T, s *Server, allow, paths []string) string {
	t.Helper()
	r, err := newTestRestrictedSession(s, allow, paths)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newTestRestrictedSession(s *Server, allow, paths []string) (string, error) {
	r, err := session.NewRestriction(allow, paths)
	if err != nil {
		return "", err
	}
	sess, err := s.sessMgr.Create(session.Config{
		Command:     testShell(),
		Args:        nil,
		Mode:        api.ModePipe,
		Name:        "restricted",
		SSHConfig:   "internal",
		Rows:        24,
		Cols:        80,
		Restriction: r,
	})
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

// createTestSession makes an ordinary unrestricted session for the guard tests.
func createTestSession(t *testing.T, s *Server, mode api.SessionMode) string {
	t.Helper()
	sess, err := s.sessMgr.Create(session.Config{
		Command:   testShell(),
		Args:      nil,
		Mode:      mode,
		Name:      "plain",
		SSHConfig: "internal",
		Rows:      24,
		Cols:      80,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

// TestGuardAllowsUnrestrictedSession pins the backward-compatibility contract:
// a session created without a restriction keeps the whole tool surface, so the
// guard is invisible to every existing caller.
func TestGuardAllowsUnrestrictedSession(t *testing.T) {
	s := newTestServer(t)
	sid := createTestSession(t, s, api.ModePipe)

	// Tools the whitelist of a skill extension would forbid all pass here.
	for _, tool := range []string{"file_write", "file_delete", "session_terminate"} {
		if bad := (toolGuard{server: s}).check(tool, makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "/etc/shadow",
		})); bad != nil {
			t.Errorf("unrestricted session: %s was refused", tool)
		}
	}
}

// TestGuardRefusesToolsOutsideWhitelist pins the real refusal: a restricted
// session returns a CodeForbidden result naming the tool, and the allowed ones
// still pass.
func TestGuardRefusesToolsOutsideWhitelist(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read", "file_stat"}, []string{"~/skills/foo/"})

	for _, tool := range []string{"file_read", "file_stat"} {
		if bad := (toolGuard{server: s}).check(tool, makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "~/skills/foo/SKILL.md",
		})); bad != nil {
			code, msg := decodeToolError(t, bad)
			t.Errorf("allowed tool %s was refused: %s (%s)", tool, msg, code)
		}
	}

	for _, tool := range []string{"file_write", "file_delete", "shell_input", "session_terminate"} {
		bad := (toolGuard{server: s}).check(tool, makeRequest(map[string]any{
			"session_id":  sid,
			"remote_path": "~/skills/foo/SKILL.md",
		}))
		if bad == nil {
			t.Fatalf("restricted session: %s should have been refused", tool)
		}
		code, msg := decodeToolError(t, bad)
		if code != CodeForbidden {
			t.Errorf("%s refusal code = %q, want %q (msg: %s)", tool, code, CodeForbidden, msg)
		}
	}
}

// TestGuardRefusesPathOutsideWhitelist pins that reachability is the gate, not
// direction: reading a path outside the tree is refused even though file_read
// itself is allowed.
func TestGuardRefusesPathOutsideWhitelist(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read"}, []string{"~/skills/foo/"})

	bad := (toolGuard{server: s}).check("file_read", makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "/etc/passwd",
	}))
	if bad == nil {
		t.Fatal("file_read outside the whitelist path should be refused")
	}
	code, msg := decodeToolError(t, bad)
	if code != CodeForbidden {
		t.Errorf("code = %q, want %q", code, CodeForbidden)
	}
	if !strings.Contains(msg, "outside") {
		t.Errorf("message should name the path problem, got %q", msg)
	}

	// The same tool on an allowed path passes.
	if bad := (toolGuard{server: s}).check("file_read", makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/scripts/run.sh",
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("file_read on an allowed path was refused: %s", msg)
	}
}

// TestGuardResolvesByShellID pins that a shell-addressed tool is gated through
// its parent session's restriction, since the restriction belongs to the
// container the shell was created under.
func TestGuardResolvesByShellID(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"shell_output"}, nil)
	sess := s.sessMgr.Get(sid)
	if sess == nil {
		t.Fatal("session not found")
	}
	shellID := sess.PrimaryShellID()
	if shellID == "" {
		t.Fatal("test session has no primary shell")
	}

	if bad := (toolGuard{server: s}).check("shell_output", makeRequest(map[string]any{
		"shell_id": shellID,
	})); bad != nil {
		_, msg := decodeToolError(t, bad)
		t.Errorf("shell_output on the session's own shell was refused: %s", msg)
	}
	if bad := (toolGuard{server: s}).check("shell_input", makeRequest(map[string]any{
		"shell_id": shellID,
	})); bad == nil {
		t.Error("shell_input should be refused on a session that allows only shell_output")
	}
}

// TestGuardIgnoresSessionlessTools pins that tools which address no existing
// session pass untouched: session_start and friends have nothing to gate
// against, and gating them would break ordinary session creation.
func TestGuardIgnoresSessionlessTools(t *testing.T) {
	s := newTestServer(t)
	// A restricted session exists to prove the guard is armed and still does not
	// fire on a call that names no target.
	guardTestSession(t, s, []string{"file_read"}, nil)

	for _, args := range []map[string]any{
		{"command": "bash"},
		{"ssh_config": "internal"},
		{},
	} {
		if bad := (toolGuard{server: s}).check("session_start", makeRequest(args)); bad != nil {
			t.Errorf("session_start with %v was refused", args)
		}
	}
}

// TestGuardUnknownSessionPasses pins that the guard does not preempt the
// handlers' own not-found errors: an id that resolves to nothing is the
// handler's business, so the guard lets it through and the refusal the caller
// sees comes from the tool with its proper code.
func TestGuardUnknownSessionPasses(t *testing.T) {
	s := newTestServer(t)
	bad := (toolGuard{server: s}).check("file_read", makeRequest(map[string]any{
		"session_id": "no-such-session",
	}))
	if bad != nil {
		t.Error("an unresolvable id should reach the handler, not be refused by the guard")
	}
}

// TestGuardThroughHandler pins the end-to-end path: the guard runs inside
// withLogging, so a call that the session's whitelist refuses fails with the
// forbidden code before the handler is entered.
func TestGuardThroughHandler(t *testing.T) {
	s := newTestServer(t)
	sid := guardTestSession(t, s, []string{"file_read"}, []string{"~/skills/foo/"})

	result, err := withLogging("file_delete", s.handleFileDelete, s)(context.Background(), makeRequest(map[string]any{
		"session_id":  sid,
		"remote_path": "~/skills/foo/x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("file_delete on a read-only extension session must be refused")
	}
	if code, _ := decodeToolError(t, result); code != CodeForbidden {
		t.Errorf("code = %q, want %q", code, CodeForbidden)
	}
}

var _ = context.Background
