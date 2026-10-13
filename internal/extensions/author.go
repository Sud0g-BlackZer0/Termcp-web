package extensions

import (
	"strings"
)

// This file is the single authoring layer. Both callers that write an
// extension template — the MCP `extension` tool and the WebUI's own create/
// update routes — apply an author's arguments through ApplyArgs and save
// through the same Store, so a rule added here reaches both surfaces at once
// and neither can drift into accepting a template the other rejects.

// ApplyArgs writes the fields an author supplied onto a template. A key that is
// absent is left alone, so an update changes only what was sent; an empty array
// is meaningful and clears the field, because "no whitelisted tools" and "no
// restriction" are different templates.
//
// update distinguishes the two call shapes: a create starts from a preset and
// takes its conventional values for anything left out, while an update already
// has the stored template behind it and must not gain preset values the author
// did not ask for.
func ApplyArgs(ext *Extension, args map[string]any, update bool) {
	if hasOpt(args, "kind") {
		if kind := strings.TrimSpace(optString(args, "kind", "")); kind != "" {
			ext.Kind = kind
		}
	}
	if hasOpt(args, "target") {
		ext.Target = strings.TrimSpace(optString(args, "target", ""))
	}
	if hasOpt(args, "description") {
		ext.Description = strings.TrimSpace(optString(args, "description", ""))
	}
	if hasOpt(args, "command") {
		ext.Command = strings.TrimSpace(optString(args, "command", ""))
	}
	if hasOpt(args, "mode") {
		ext.Mode = strings.TrimSpace(optString(args, "mode", ""))
	}
	if hasOpt(args, "on_exit") {
		ext.OnExit = strings.TrimSpace(strings.ToLower(optString(args, "on_exit", "")))
	}
	if hasOpt(args, "args") {
		ext.Args = optStringSlice(args, "args")
	}
	if hasOpt(args, "allow") {
		ext.Allow = optStringSlice(args, "allow")
	}
	if hasOpt(args, "paths") {
		ext.Paths = optStringSlice(args, "paths")
	}
	if hasOpt(args, "notes") {
		ext.Notes = optString(args, "notes", "")
	}
	if hasOpt(args, "enabled") {
		ext.SetEnabled(optBool(args, "enabled", true))
	}
	if hasOpt(args, "rows") {
		ext.Rows = optInt(args, "rows", 0)
	}
	if hasOpt(args, "cols") {
		ext.Cols = optInt(args, "cols", 0)
	}
	if hasOpt(args, "forwards") {
		ext.Forwards = ParseForwardDecls(args["forwards"])
	}
	if !update {
		ApplyDefaults(ext)
	}
	ext.Normalize()
}

// SaveTemplate validates and persists one template. It is the only write path
// both callers use, so a template the WebUI accepts is a template the MCP tool
// accepts, and vice versa.
func SaveTemplate(store *Store, ext *Extension) error {
	if err := ext.Validate(); err != nil {
		return err
	}
	return store.Save(ext)
}

// ParseForwardDecls turns the JSON array an author sent into forward
// declarations. An item that is not an object is dropped rather than failing the
// whole save: the Validate pass is what reports a bad template.
func ParseForwardDecls(v any) []ForwardDecl {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]ForwardDecl, 0, len(arr))
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, ForwardDecl{
			Kind:       optString(m, "kind", ""),
			LocalHost:  optString(m, "local_host", ""),
			LocalPort:  optInt(m, "local_port", 0),
			RemoteHost: optString(m, "remote_host", ""),
			RemotePort: optInt(m, "remote_port", 0),
		})
	}
	return out
}

/* The option readers below are deliberately narrower than a general JSON getter:
   each one accepts only the type the field's author sends, and anything else
   falls back to the default rather than coercing. A form that sends "8080" as a
   string therefore keeps the stored value, which is the honest outcome for a
   field the caller did not send in the shape the template asks for. */

func hasOpt(args map[string]any, key string) bool {
	_, ok := args[key]
	return ok
}

func optString(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func optBool(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func optInt(args map[string]any, key string, def int) int {
	if v, ok := args[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
	}
	return def
}

func optStringSlice(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	switch arr := v.(type) {
	case []any:
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), arr...)
	default:
		return nil
	}
}
