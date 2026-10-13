package webui

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// extensionSummary is one row of the right panel. It carries what a browser
// needs to render the card without a second fetch: the template's shape, the
// reconciler's verdict on it, and the handles an available extension is driven
// through.
type extensionSummary struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Target      string `json:"target"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Status      string `json:"status"`
	// SessionID/ShellID are the handles a click opens. Empty when the extension
	// is not available, which is exactly the case the card has to explain.
	SessionID string `json:"session_id,omitempty"`
	ShellID   string `json:"shell_id,omitempty"`
	// Forwards are the tunnels the extension declared, with the address each one
	// is listening on once it is up.
	Forwards []extensions.ForwardStatus `json:"forwards,omitempty"`
	// Failure is the most recent reason an extension is not up. One line, not
	// the whole history: the panel shows the verdict, the API serves the rest.
	Failure *extensions.Failure `json:"failure,omitempty"`
	// NextRetryMs is when the reconciler tries again (Unix ms), 0 = no retry
	// scheduled. The panel renders it as a countdown for a human waiting on it.
	NextRetryMs int64 `json:"next_retry_ms,omitempty"`
}

// summarizeExtension folds a template and its reconciler status into one panel
// row. A template whose state the reconciler has not reached yet reports
// unavailable — an extension starts down and has to be proved up.
func summarizeExtension(ext *extensions.Extension, st extensions.Status) extensionSummary {
	out := extensionSummary{
		Name:        ext.Name,
		Kind:        ext.Kind,
		Target:      ext.Target,
		Description: ext.Description,
		Enabled:     ext.IsEnabled(),
		Status:      st.Status,
		SessionID:   st.SessionID,
		ShellID:     st.ShellID,
		Forwards:    st.Forwards,
		NextRetryMs: st.NextRetry,
	}
	if n := len(st.Failures); n > 0 {
		out.Failure = &st.Failures[n-1]
	}
	if out.Status == "" {
		out.Status = extensions.StatusUnavailable
	}
	return out
}

// summarizeExtensions folds every stored template and its reconciler status into
// panel rows. A template whose state the reconciler has not reached yet reports
// unavailable — an extension starts down and has to be proved up — and one whose
// file no longer parses is listed anyway, with whatever failure the reconciler
// recorded on it rather than as a gap in the panel.
//
// It is the single builder for both surfaces that show the list: the REST read
// and the WebSocket frame. The socket marshals once for every open tab while the
// REST call answers one request, which is the only thing the two do differently.
func summarizeExtensions(r *extensions.Reconciler) []extensionSummary {
	names, err := r.Store().List()
	if err != nil {
		return nil
	}
	rows := make([]extensionSummary, 0, len(names))
	for _, name := range names {
		st, _ := r.Status(name)
		ext, err := r.Store().Load(name)
		if err != nil {
			rows = append(rows, extensionSummary{
				Name:    name,
				Enabled: true,
				Status:  firstNonEmpty(st.Status, extensions.StatusUnavailable),
			})
			continue
		}
		rows = append(rows, summarizeExtension(ext, st))
	}
	return rows
}

func (h *Handler) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"extensions": []extensionSummary{}})
		return
	}
	rows := summarizeExtensions(h.Extensions)
	if rows == nil {
		rows = []extensionSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"extensions": rows})
}

func (h *Handler) handleGetExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	name := r.PathValue("name")
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st, _ := h.Extensions.Status(name)
	// The detail view serves the whole template, notes included: the panel's
	// editor and the failure history both live behind this one call.
	writeJSON(w, http.StatusOK, map[string]any{
		"extension":     summarizeExtension(ext, st),
		"kind":          ext.Kind,
		"mode":          ext.Mode,
		"command":       ext.Command,
		"args":          ext.Args,
		"on_exit":       ext.OnExit,
		"allow":         ext.Allow,
		"paths":         ext.Paths,
		"notes":         ext.Notes,
		"failures":      st.Failures,
		"forward_decls": ext.Forwards,
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

/* The three authoring routes. They are the browser's half of the same authoring
   layer the MCP `extension` tool uses — both decode the author's fields into a
   map, hand it to extensions.ApplyArgs, and persist through
   extensions.SaveTemplate. A field an author omits keeps its stored value on
   update and the preset's value on create; a field present but empty clears it.
   That is the whole semantic contract, and because it lives in
   internal/extensions the two callers cannot drift apart on it. */

// readExtensionBody is the shared decode for create and update. A body over the
// limit is refused rather than truncated: a truncated template would validate
// against fields the author did not send.
func readExtensionBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, extensions.MaxConfigSize+1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if len(body) > extensions.MaxConfigSize {
		http.Error(w, "extension template too large", http.StatusRequestEntityTooLarge)
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, true
	}
	var args map[string]any
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return nil, false
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, true
}

func extensionName(r *http.Request) string {
	return strings.TrimSpace(r.PathValue("name"))
}

// bodyName is the create route's name: the file's directory is not in the path
// on a POST, so the author names it in the body. A non-string is refused rather
// than stringified — a name an author sent as a number is not a name.
func bodyName(args map[string]any) (string, bool) {
	if v, ok := args["name"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
	return "", true
}

// bodyKind is the preset to start a create from; an absent or empty kind means a
// custom template with no preset values of its own.
func bodyKind(args map[string]any) string {
	if v, ok := args["kind"]; ok {
		if s, _ := v.(string); strings.TrimSpace(s) != "" {
			return s
		}
	}
	return extensions.KindCustom
}

func (h *Handler) handleCreateExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	args, ok := readExtensionBody(w, r)
	if !ok {
		return
	}
	name, isString := bodyName(args)
	if !isString {
		http.Error(w, "name must be a string", http.StatusBadRequest)
		return
	}
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if _, err := h.Extensions.Store().Load(name); err == nil {
		http.Error(w, "extension "+name+" already exists; use PUT to change it", http.StatusConflict)
		return
	}
	ext := extensions.Preset(bodyKind(args))
	ext.Name = name
	extensions.ApplyArgs(&ext, args, false)
	if err := extensions.SaveTemplate(h.Extensions.Store(), &ext); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, _ := h.Extensions.Status(name)
	writeJSON(w, http.StatusCreated, map[string]any{"extension": summarizeExtension(&ext, st)})
}

func (h *Handler) handleUpdateExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	name := extensionName(r)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	ext, err := h.Extensions.Store().Load(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	args, ok := readExtensionBody(w, r)
	if !ok {
		return
	}
	extensions.ApplyArgs(ext, args, true)
	if err := extensions.SaveTemplate(h.Extensions.Store(), ext); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	st, _ := h.Extensions.Status(name)
	writeJSON(w, http.StatusOK, map[string]any{"extension": summarizeExtension(ext, st)})
}

func (h *Handler) handleDeleteExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		http.Error(w, "extensions are not configured", http.StatusServiceUnavailable)
		return
	}
	name := extensionName(r)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := h.Extensions.Store().Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRetryExtension is the human's nudge. It is the same call the agent's
// extension(action=retry) makes, so the two cannot drift apart in what they are
// allowed to do: the panel button and the tool are one reconciler entry point
// with two callers.
func (h *Handler) handleRetryExtension(w http.ResponseWriter, r *http.Request) {
	if h.Extensions == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "extensions are not configured"})
		return
	}
	name := r.PathValue("name")
	if err := h.Extensions.Retry(name); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
