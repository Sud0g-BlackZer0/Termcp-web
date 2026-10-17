package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The client list answers "who is attached to this instance?" — the question the
// issue asks. Two kinds of caller exist and they are genuinely different: an MCP
// client is an agent driving sessions and announces a name during initialize; a
// Web UI viewer is a browser tab holding a socket. What the tests below pin is
// the part that is invisible in a browser smoke test and easy to get wrong:
//
//   - a viewer is listed with the address it came from, port stripped, so one
//     person on one machine is one row however many ephemeral ports they use;
//   - the watched-session count is read at list time, not captured when the
//     socket opened (it changes constantly, and a stale count would be a lie);
//   - a closed tab disappears;
//   - the MCP half is reported as the MCP server sees it, and an instance with no
//     MCP server says so instead of returning null.

func TestViewerRegistryListsAttachedTabs(t *testing.T) {
	reg := newViewerRegistry()

	watching := 0
	id := reg.add("10.0.0.5:54321", "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0", func() int { return watching })
	if got := reg.list(); len(got) != 1 {
		t.Fatalf("one tab attached, got %d viewers", len(got))
	} else {
		v := got[0]
		// The port is per-connection noise: the same person reloading gets a new
		// one, and listing it would make one viewer look like many.
		if v.Addr != "10.0.0.5" {
			t.Errorf("addr = %q, want the host without its port", v.Addr)
		}
		if v.UserAgent == "" {
			t.Error("the user agent identifies the tab; it must be listed")
		}
		if v.Since == "" {
			t.Error("a viewer needs a since timestamp")
		}
	}

	// The count follows what the tab is streaming now, not what it was holding
	// when it attached.
	watching = 3
	if got := reg.list()[0].Watching; got != 3 {
		t.Errorf("watching = %d, want the current 3 (the count must be read, not captured)", got)
	}
	watching = 0
	if got := reg.list()[0].Watching; got != 0 {
		t.Errorf("watching = %d, want 0 after the tab stopped streaming", got)
	}

	// Oldest first, so the panel does not reshuffle on every frame.
	time.Sleep(2 * time.Millisecond)
	reg.add("10.0.0.9:1234", "second", func() int { return 1 })
	got := reg.list()
	if len(got) != 2 || got[0].Addr != "10.0.0.5" {
		t.Fatalf("viewers must be oldest-first; got %+v", got)
	}

	// A closed tab is gone.
	reg.remove(id)
	got = reg.list()
	if len(got) != 1 || got[0].Addr != "10.0.0.9" {
		t.Fatalf("the closed tab is still listed: %+v", got)
	}
}

// A user agent is attacker-controlled and unbounded; the panel must not render it
// whole.
func TestViewerUserAgentIsBounded(t *testing.T) {
	reg := newViewerRegistry()
	reg.add("1.2.3.4:1", string(make([]byte, 4096)), nil)
	if got := reg.list()[0].UserAgent; len(got) > 200 {
		t.Fatalf("user agent was not truncated: %d bytes", len(got))
	}
}

// The MCP half reaches the API through the Handler's hook, and an instance with no
// MCP server must answer with an empty list rather than null — a client rendering
// the response should not have to special-case absent keys.
func TestHandlerListsClients(t *testing.T) {
	h := &Handler{}
	if got := h.Viewers(); len(got) != 0 {
		t.Errorf("a fresh handler has no viewers, got %d", len(got))
	}

	rr := httptest.NewRecorder()
	h.handleListClients(rr, httptest.NewRequest(http.MethodGet, "/api/clients", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	served := decodeBody(t, rr)
	if _, ok := served["mcp"].([]any); !ok {
		t.Errorf("mcp must be a JSON array, got %T", served["mcp"])
	}
	if _, ok := served["webui"].([]any); !ok {
		t.Errorf("webui must be a JSON array, got %T", served["webui"])
	}

	// With an MCP server, its clients are reported as-is.
	h.MCPConnections = func() []any {
		return []any{map[string]any{
			"session_id":   "sess-abc",
			"name":         "claude-ai",
			"version":      "0.1.0",
			"namespace":    "ns:alice",
			"connected_at": "2026-10-17T00:00:00Z",
		}}
	}
	rr = httptest.NewRecorder()
	h.handleListClients(rr, httptest.NewRequest(http.MethodGet, "/api/clients", nil))
	served = decodeBody(t, rr)
	rows, _ := served["mcp"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 MCP client, got %d", len(rows))
	}
	row, _ := rows[0].(map[string]any)
	for field, want := range map[string]string{
		"name": "claude-ai", "version": "0.1.0",
		"namespace": "ns:alice", "session_id": "sess-abc",
	} {
		if got, _ := row[field].(string); got != want {
			t.Errorf("%s = %q, want %q", field, got, want)
		}
	}
}

// decodeBody unmarshals a response body for the assertions above.
func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad JSON %q: %v", rr.Body.String(), err)
	}
	return m
}
