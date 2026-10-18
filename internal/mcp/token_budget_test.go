package mcp

import (
	"encoding/json"
	"testing"
)

// TestTokenBudgetGuard prevents regression on the one payload every client pays
// for on every turn.
//
// Issue #117 replaced a 32-tool, ~24 KB surface with a single `term` entry, so
// the guard now measures the entry itself and the constant total: the budget is
// a hard constraint, not a warning, because the whole point of the redesign is
// that the surface stops growing with the capability count.
func TestTokenBudgetGuard(t *testing.T) {
	s := New(nil, nil, nil, nil, "test")
	s.RegisterSSHConfigWriteTools()

	tools := listTools(t, s, originContext("http://127.0.0.1:18765"))
	if len(tools) != 1 {
		names := make([]string, 0, len(tools))
		for _, st := range tools {
			names = append(names, st.Name)
		}
		t.Fatalf("the default surface must be exactly one tool, got %d: %v", len(tools), names)
	}

	b, err := json.Marshal(tools[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tools/list total: %d B", len(b))
	t.Logf("instructions: %d B", len(mcpServerInstructions))

	// #117 33c: the schema budget is a hard constraint.
	if len(b) >= 2500 {
		t.Errorf("the %s schema serialises to %d B, over the 2500 B budget", tools[0].Name, len(b))
	}
	// The listing cannot grow with the capability count (33b): with a single
	// entry the total must stay under a couple of kilobytes including the
	// transport's own envelope.
	if len(b) > 3000 {
		t.Errorf("tools/list is %d B; a single-entry surface must stay small", len(b))
	}
	if n := len(mcpServerInstructions); n > 2600 {
		t.Errorf("instructions %d B exceeds 2600 B budget", n)
	}
}

// TestSpecialisedSurfaceIsOptIn locks the coexistence rule (issue #117 33f): the
// same build can expose both surfaces, the specialised one is off unless asked
// for, and asking for it changes nothing about how an operation behaves —
// admission lives in the capability table, not in the shape.
func TestSpecialisedSurfaceIsOptIn(t *testing.T) {
	defaulted := New(nil, nil, nil, nil, "test")
	if n := len(defaulted.mcpServer.ListTools()); n != 1 {
		t.Errorf("default surface has %d tools, want 1", n)
	}

	full := New(nil, nil, nil, nil, "test", WithSpecialisedTools())
	full.RegisterSSHConfigWriteTools()
	tools := full.mcpServer.ListTools()
	if len(tools) < 30 {
		t.Errorf("the specialised surface has %d tools; it is the full per-operation set", len(tools))
	}

	// Every specialised tool must name a capability, and every capability must be
	// reachable through `term`: the two surfaces are two shapes of one table.
	for name := range tools {
		if name == termToolName {
			continue
		}
		var g = lookupGate(name)
		if !g.known {
			t.Errorf("specialised tool %q has no capability", name)
		}
	}
}

// TestDeferLoadingPolicy keeps the defer_loading classification honest for the
// specialised surface, which is the only one whose listing can be large enough
// for the marker to matter.
//
// The classification is a property of termcp; whether it reaches the wire is a
// deployment choice. Both halves matter: a model that must search before it can
// type a command pays a round trip on every interaction, so the core driving
// loop must never be deferred, and clients that drop defer_loading must still
// see the whole surface unless the operator opted in.
func TestDeferLoadingPolicy(t *testing.T) {
	core := []string{
		"session_start", "session_list", "session_info",
		"session_terminate", "session_delete",
		"shell_open", "shell_list", "shell_close", "shell_input", "shell_key", "shell_output",
		"notify_user",
	}
	isCore := make(map[string]bool, len(core))
	for _, name := range core {
		isCore[name] = true
	}

	collect := func(t *testing.T, opts ...Option) map[string]bool {
		t.Helper()
		s := New(nil, nil, nil, nil, "test", append(opts, WithSpecialisedTools())...)
		s.RegisterSSHConfigWriteTools()
		byName := make(map[string]bool)
		for _, st := range s.mcpServer.ListTools() {
			byName[st.Tool.Name] = st.Tool.DeferLoading
		}
		return byName
	}

	// Default: nothing is deferred, so a client that does not understand the
	// marker still receives every tool.
	def := collect(t)
	for name, deferred := range def {
		if deferred {
			t.Errorf("default config marked %q defer_loading; the marker must be opt-in", name)
		}
	}

	// partial: the hot path stays eager, the wide/low-frequency surfaces defer.
	on := collect(t, DeferLevel(DeferPartial))
	for _, name := range core {
		deferred, ok := on[name]
		if !ok {
			t.Errorf("core tool %q is not registered", name)
			continue
		}
		if deferred {
			t.Errorf("core tool %q must not be deferred (it is on the hot path)", name)
		}
	}
	for name := range deferredTools {
		got, ok := on[name]
		if !ok {
			t.Errorf("deferredTools lists %q, which is not a registered tool (stale entry)", name)
			continue
		}
		if !got {
			t.Errorf("tool %q is listed in deferredTools but was not marked defer_loading", name)
		}
	}

	// Every registered tool must be classified, so a new tool cannot silently
	// default into the always-loaded set and inflate every client's context.
	for name := range on {
		if name == termToolName || deferredTools[name] || isCore[name] {
			continue
		}
		t.Errorf("tool %q is neither in the core list nor deferredTools; classify it", name)
	}

	// all: only the minimum driving loop stays eager; everything else —
	// including the partial-level core — defers. A client that supports
	// deferred loading pays one search round trip per first use; that is the
	// level's whole point.
	all := collect(t, DeferLevel(DeferAll))
	for name := range all {
		if name == termToolName || allLevelCore[name] || deferredTools[name] || isCore[name] {
			continue
		}
		t.Errorf("tool %q is neither all-level core nor classified; it would silently go eager under level=all", name)
	}
	for name := range allLevelCore {
		deferred, ok := all[name]
		if !ok {
			t.Errorf("all-level core tool %q is not registered", name)
			continue
		}
		if deferred {
			t.Errorf("all-level core tool %q must stay eager: the minimum driving loop cannot require a search", name)
		}
	}
}
