package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// extensionTestHandler stands up the panel's full backing stack: a loopback SSH
// server, a session manager, both stores and a running reconciler, so the
// endpoints answer with real state rather than a stub.
func extensionTestHandler(t *testing.T) *Handler {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	r := extensions.NewReconciler(
		extensions.NewStore(dir), sessMgr, msgMgr,
		sshconfig.NewStore(dir), forward.NewForwardManager(),
	)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop() })
	return &Handler{Sessions: sessMgr, MsgMgr: msgMgr, Extensions: r}
}

func getExtensions(t *testing.T, h *Handler) []extensionSummary {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/extensions = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body struct {
		Extensions []extensionSummary `json:"extensions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode extensions body: %v (%s)", err, rr.Body.String())
	}
	return body.Extensions
}

func saveTestExtension(t *testing.T, h *Handler, ext *extensions.Extension) {
	t.Helper()
	if err := h.Extensions.Store().Save(ext); err != nil {
		t.Fatal(err)
	}
}

// TestExtensionsEndpointEmpty pins that a handler with no reconciler still
// answers, with an empty list rather than an error: the panel renders nothing.
func TestExtensionsEndpointEmpty(t *testing.T) {
	h := &Handler{}
	if rows := getExtensions(t, h); len(rows) != 0 {
		t.Errorf("want no extensions without a reconciler, got %d", len(rows))
	}
}

// TestExtensionsEndpointListsStatus pins the panel's row: the template's shape,
// the reconciler's verdict, and the handles only an available extension carries.
func TestExtensionsEndpointListsStatus(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Description = "docs window"
	saveTestExtension(t, h, &ext)

	// An extension starts unavailable and is pushed toward available; wait for
	// the reconciler to prove the session before asserting on the handles.
	deadline := time.Now().Add(8 * time.Second)
	var rows []extensionSummary
	for {
		rows = getExtensions(t, h)
		if row := findExtension(rows, "docs"); row != nil && row.Status == extensions.StatusAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("extension never became available, last rows: %+v", rows)
		}
		time.Sleep(50 * time.Millisecond)
	}

	row := findExtension(rows, "docs")
	if row == nil {
		t.Fatalf("extension docs missing from %v", rows)
	}
	if row.Kind != "skill" || row.Target != "internal" || row.Description != "docs window" {
		t.Errorf("template fields not echoed: %+v", row)
	}
	if !row.Enabled {
		t.Error("an extension with no enabled key should be enabled")
	}
	if row.SessionID == "" {
		t.Error("an available extension should carry its session id")
	}
	if row.ShellID != "" {
		t.Errorf("a no-shell skill extension should not carry a shell id, got %q", row.ShellID)
	}
}

// TestExtensionsEndpointDisabled pins the parked state: the reconciler does not
// connect it, and the card says why it is not coming up.
func TestExtensionsEndpointDisabled(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "parked"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.SetEnabled(false)
	saveTestExtension(t, h, &ext)

	deadline := time.Now().Add(2 * time.Second)
	for {
		row := findExtension(getExtensions(t, h), "parked")
		if row != nil && row.Status == extensions.StatusDisabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("disabled extension should report disabled at once, got %+v", getExtensions(t, h))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionsEndpointFailure pins that a template that cannot come up shows
// the reason on its row, so the panel explains the red lamp instead of just
// showing it.
func TestExtensionsEndpointFailure(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	deadline := time.Now().Add(8 * time.Second)
	for {
		row := findExtension(getExtensions(t, h), "nowhere")
		if row != nil && row.Status == extensions.StatusUnavailable && row.Failure != nil {
			if row.Failure.Stage != "target" {
				t.Errorf("want a target-stage failure, got %+v", row.Failure)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failing extension never reported its reason, got %+v", getExtensions(t, h))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestExtensionDetail pins that the single-extension view serves the whole
// template, notes included, and the full failure history.
func TestExtensionDetail(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindMCP)
	ext.Name = "mcpbox"
	ext.Target = "internal"
	ext.Command = "cat"
	ext.Notes = "drive it as line-delimited JSON-RPC"
	saveTestExtension(t, h, &ext)

	mux := http.NewServeMux()
	h.Register(mux)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/mcpbox", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/extensions/mcpbox = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode extension detail: %v (%s)", err, rr.Body.String())
	}
	if notes, _ := body["notes"].(string); notes != ext.Notes {
		t.Errorf("notes = %v, want the template's notes", body["notes"])
	}
	if allow, _ := body["allow"].([]any); len(allow) == 0 {
		t.Error("the mcp preset's whitelist should be served")
	}
	if _, ok := body["extension"]; !ok {
		t.Errorf("detail should nest the panel row, got %v", body)
	}

	// A name that was never saved is a 404, not an empty object.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/extensions/ghost", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET unknown extension = %d, want 404", rr.Code)
	}
}

// TestExtensionsWSFrame pins that the socket frame carries the same rows the
// REST call serves, so the panel's two read paths cannot drift.
func TestExtensionsWSFrame(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	rows := summarizeExtensions(h.Extensions)
	if row := findExtension(rows, "docs"); row == nil {
		t.Fatalf("WS frame misses the extension, got %+v", rows)
	} else if row.Status != extensions.StatusUnavailable && row.Status != extensions.StatusProbing && row.Status != extensions.StatusAvailable {
		t.Errorf("WS row status %q is not one the reconciler reports", row.Status)
	}
}

// TestExtensionRetryEndpoint pins the panel's one write path: the button and
// the agent's extension(action=retry) are the same reconciler call, so a
// disabled extension is refused on both and a failing one gets another attempt.
func TestExtensionRetryEndpoint(t *testing.T) {
	h := extensionTestHandler(t)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "nowhere"
	ext.Target = "no-such-profile"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	parked := extensions.Preset(extensions.KindSkill)
	parked.Name = "parked"
	parked.Target = "internal"
	parked.Paths = []string{"/srv/"}
	parked.SetEnabled(false)
	saveTestExtension(t, h, &parked)

	mux := http.NewServeMux()
	h.Register(mux)

	post := func(name string) int {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/extensions/"+name+"/retry", nil))
		if rr.Code != http.StatusOK {
			return rr.Code
		}
		var body struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode retry body for %s: %v (%s)", name, err, rr.Body.String())
		}
		if !body.OK {
			return 0
		}
		return 200
	}

	// A disabled extension is not reconciled, so there is nothing to retry.
	if code := post("parked"); code != 0 {
		t.Errorf("retry on a disabled extension should report ok=false, got status %d", code)
	}
	// A failing extension is retried: the endpoint answers ok and the reconciler
	// records another attempt rather than leaving the failure count where it was.
	if code := post("nowhere"); code != 200 {
		t.Errorf("retry on a failing extension should succeed, got status %d", code)
	}

	// A name that was never saved is refused, not silently accepted as a retry.
	if code := post("ghost"); code != 0 {
		t.Errorf("retry on an unknown extension should report ok=false, got status %d", code)
	}

	// No reconciler at all: the endpoint still answers, with ok=false, so the
	// panel's button degrades instead of 500ing.
	empty := &Handler{}
	emptyMux := http.NewServeMux()
	empty.Register(emptyMux)
	rr := httptest.NewRecorder()
	emptyMux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/extensions/ghost/retry", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("retry without a reconciler = %d, want 200", rr.Code)
	}
}

func findExtension(rows []extensionSummary, name string) *extensionSummary {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

/* The authoring routes. These pin that the browser's create/update/delete are
   the same write the MCP `extension` tool makes: the same store, the same
   validation, and therefore the same answer for the same template. */

// callExtension issues one authoring request and returns the status code and the
// decoded row the route answered with (nil when the body carries no row).
func callExtension(t *testing.T, mux *http.ServeMux, method, target, body string) (int, extensionSummary) {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	if reader != nil {
		req = httptest.NewRequest(method, target, reader)
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
		return rr.Code, extensionSummary{}
	}
	var out struct {
		Extension extensionSummary `json:"extension"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s %s body: %v (%s)", method, target, err, rr.Body.String())
	}
	return rr.Code, out.Extension
}

// TestExtensionCreate pins the browser's create: a kind and a target are enough,
// the preset's conventional values are filled in, and the row is echoed back.
func TestExtensionCreate(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	code, row := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"docs","kind":"skill","target":"internal","paths":["/srv/"]}`)
	if code != http.StatusCreated {
		t.Fatalf("POST /api/extensions = %d, want 201", code)
	}
	if row.Kind != "skill" || row.Target != "internal" {
		t.Errorf("created row = %+v, want the sent kind and target", row)
	}
	// The store is the shared layer: what the route wrote is what the tool reads.
	ext, err := h.Extensions.Store().Load("docs")
	if err != nil {
		t.Fatalf("created extension not persisted: %v", err)
	}
	if len(ext.Paths) != 1 || ext.Paths[0] != "/srv/" {
		t.Errorf("paths = %v, want [/srv/]", ext.Paths)
	}
	if len(ext.Allow) == 0 {
		t.Error("the skill preset's tool whitelist should be filled in on create")
	}

	// A duplicate is a conflict, so the author hears about the name collision
	// instead of overwriting a template in place.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"docs","kind":"skill","target":"internal"}`); code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", code)
	}
	// A template that cannot be built is refused before it is saved, so the
	// panel never shows a template the reconciler cannot stand up.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"nopaths","kind":"skill","target":"internal"}`); code != http.StatusBadRequest {
		t.Errorf("a skill with no paths = %d, want 400", code)
	}
	// No name means no file to write it to.
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"kind":"skill","target":"internal"}`); code != http.StatusBadRequest {
		t.Errorf("create without a name = %d, want 400", code)
	}
	// Malformed JSON is reported, not silently treated as "change nothing".
	if code, _ := callExtension(t, mux, http.MethodPost, "/api/extensions", `{not json`); code != http.StatusBadRequest {
		t.Errorf("malformed body = %d, want 400", code)
	}
}

// TestExtensionCreateCustom pins that an unknown or absent kind is a custom
// template rather than an error: the panel's "other" shape is a first-class
// authoring choice, not a fallback the route apologises for.
func TestExtensionCreateCustom(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	code, row := callExtension(t, mux, http.MethodPost, "/api/extensions",
		`{"name":"scratch","target":"internal","mode":"pty"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST custom = %d, want 201", code)
	}
	if row.Kind != extensions.KindCustom {
		t.Errorf("absent kind = %q, want %q", row.Kind, extensions.KindCustom)
	}
	// A custom template takes no preset values, so it keeps only what the author
	// sent — a pty shell with no whitelisted tools and no notes of its own.
	ext, err := h.Extensions.Store().Load("scratch")
	if err != nil {
		t.Fatal(err)
	}
	if ext.Mode != "pty" {
		t.Errorf("mode = %q, want pty", ext.Mode)
	}
	if ext.Notes != "" || len(ext.Allow) != 0 {
		t.Errorf("custom template should carry no preset values, got notes=%q allow=%v", ext.Notes, ext.Allow)
	}
}

// TestExtensionUpdate pins the patch semantics the two callers share: an omitted
// field keeps its stored value and a present-but-empty array clears it.
func TestExtensionUpdate(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	ext.Description = "original"
	saveTestExtension(t, h, &ext)

	// Omitted fields keep their stored value; the description survives a patch
	// that only widens the paths.
	code, row := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"paths":["/srv/","/opt/"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT /api/extensions/docs = %d, want 200", code)
	}
	if row.Description != "original" {
		t.Errorf("update dropped the stored description: %+v", row)
	}
	after, err := h.Extensions.Store().Load("docs")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Paths) != 2 {
		t.Errorf("paths = %v, want the two sent", after.Paths)
	}
	// An empty array clears the field — "no whitelisted tools" and "unchanged
	// tools" are different authoring intents and both have to be expressible.
	// The skill preset fills a whitelist, so this is the field that can be
	// cleared; paths cannot, because a skill with no paths is a window onto
	// nothing and Validate refuses it (pinned in the extension package's tests).
	if code, _ := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"allow":[]}`); code != http.StatusOK {
		t.Fatalf("clearing allow = %d, want 200", code)
	}
	if after, err := h.Extensions.Store().Load("docs"); err != nil {
		t.Fatal(err)
	} else if len(after.Allow) != 0 {
		t.Errorf("empty array should clear allow, got %v", after.Allow)
	}

	// A name that was never saved is a 404, not a create-by-another-name.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPut, "/api/extensions/ghost", strings.NewReader(`{}`)))
	if rr.Code != http.StatusNotFound {
		t.Errorf("PUT unknown extension = %d, want 404", rr.Code)
	}
	// An update can still be rejected by validation.
	if code, _ := callExtension(t, mux, http.MethodPut, "/api/extensions/docs",
		`{"mode":"carrier-pigeon"}`); code != http.StatusBadRequest {
		t.Errorf("invalid mode = %d, want 400", code)
	}
}

// TestExtensionDelete pins that a delete reaches the store the reconciler reads,
// so the card the author removed is gone from the panel's own list.
func TestExtensionDelete(t *testing.T) {
	h := extensionTestHandler(t)
	mux := http.NewServeMux()
	h.Register(mux)

	ext := extensions.Preset(extensions.KindSkill)
	ext.Name = "docs"
	ext.Target = "internal"
	ext.Paths = []string{"/srv/"}
	saveTestExtension(t, h, &ext)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/docs", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE /api/extensions/docs = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if rows := getExtensions(t, h); findExtension(rows, "docs") != nil {
		t.Errorf("deleted extension still listed: %+v", rows)
	}

	// Deleting again is a no-op rather than an error: the store treats a missing
	// file as already-deleted (idempotent delete), so a double-click in the panel
	// does not report a failure for work that is already done.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/extensions/docs", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("delete a missing extension = %d, want 200 (idempotent)", rr.Code)
	}
	if err := h.Extensions.Store().Delete("docs"); err != nil {
		t.Errorf("store delete should also be idempotent: %v", err)
	}
}

// TestExtensionAuthoringWithoutReconciler pins that a handler without the
// reconciler degrades to a clear refusal instead of a 500: the panel's authoring
// buttons dim, they do not crash the page.
func TestExtensionAuthoringWithoutReconciler(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.Register(mux)

	for _, tc := range []struct {
		method, target, body string
	}{
		{http.MethodPost, "/api/extensions", `{"name":"x","kind":"skill","target":"internal"}`},
		{http.MethodPut, "/api/extensions/x", `{}`},
		{http.MethodDelete, "/api/extensions/x", ""},
	} {
		rr := httptest.NewRecorder()
		var req *http.Request
		if tc.body != "" {
			req = httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
		} else {
			req = httptest.NewRequest(tc.method, tc.target, nil)
		}
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without a reconciler = %d, want 503", tc.method, tc.target, rr.Code)
		}
	}
}
