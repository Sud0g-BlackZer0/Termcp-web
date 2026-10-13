package mcp

import (
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// toolGuard enforces a session's Restriction before a tool handler runs. It is
// the single choke point for every tool that acts on an existing session or
// shell: the wrapper in withLogging resolves the target from the call's own
// arguments, so the twenty-odd handlers do not each re-implement the check —
// and a tool added later is covered by construction instead of by memory.
//
// An ordinary session has an unrestricted (zero-value) Restriction and the guard
// never fires, so the cost for the common path is one map lookup per argument
// name. A restricted session is a strict subset of that surface.
type toolGuard struct {
	server *Server
}

// check returns a tool error result when the call must be refused, or nil when
// it may proceed. toolName is the registered tool name; request carries the
// arguments the guard resolves the target from.
//
// Only arguments that actually name the target are consulted, and a tool that
// takes neither session_id nor shell_id passes through untouched — some tools
// (session_start, shell_detect, the extension tools themselves) legitimately
// operate outside any existing session, and gating them would have nothing to
// gate against.
func (g toolGuard) check(toolName string, request mcpgo.CallToolRequest) *mcpgo.CallToolResult {
	args := request.GetArguments()
	if args == nil || g.server == nil {
		return nil
	}

	// A shell-addressed tool resolves through its parent session, since the
	// restriction is a property of the container the shell was created under.
	// shell_id also accepts a session id or locator (see requireShell), so the
	// lookup falls back to the session the way the tool handlers do.
	if shellID := getString(args, "shell_id", ""); shellID != "" {
		var sess *session.Session
		if cs := g.server.sessMgr.GetChildShell(shellID); cs != nil {
			sess = g.server.sessMgr.Get(cs.ParentSessionID())
		} else {
			sess = g.server.sessMgr.GetByShellID(shellID)
		}
		if sess == nil {
			return nil // an unresolvable id is the handler's error to report
		}
		return g.checkSession(sess, toolName, args)
	}

	if sessionID := getString(args, "session_id", ""); sessionID != "" {
		// A batch (comma-separated ids) is refused wholesale when any entry is
		// restricted: running part of a batch against a session the caller may not
		// touch would make the refusal partial and the outcome confusing.
		if strings.Contains(sessionID, ",") {
			for _, id := range strings.Split(sessionID, ",") {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				sess := g.server.sessMgr.Get(id)
				if sess == nil {
					continue
				}
				if bad := g.checkSession(sess, toolName, args); bad != nil {
					return bad
				}
			}
			return nil
		}
		sess := g.server.sessMgr.Get(sessionID)
		if sess == nil {
			return nil
		}
		return g.checkSession(sess, toolName, args)
	}
	return nil
}

// checkSession applies one session's whitelist to the call: the tool itself,
// then every path argument the file tools carry.
func (g toolGuard) checkSession(sess *session.Session, toolName string, args map[string]any) *mcpgo.CallToolResult {
	r := sess.Restriction()
	if !r.AllowsTool(toolName) {
		return toolError(CodeForbidden, "%s", forbiddenToolMessage(toolName))
	}
	// Path arguments come in several names across the file tools (remote_path,
	// from_path/to_path), and reading is as gated as writing: the whitelist is
	// about reachability, not direction.
	for _, key := range pathArgKeys {
		path := getString(args, key, "")
		if path == "" {
			continue
		}
		if !r.AllowsPath(path) {
			return toolError(CodeForbidden, "%s", forbiddenPathMessage(key, path))
		}
	}
	return nil
}

// pathArgKeys are the argument names that name a remote path. The guard checks
// each one a tool happens to carry, so a new file tool is covered by naming its
// path argument the same way. Every path argument of every file tool is here:
// file_link alone carries three (target, link_path, existing_path, new_path).
var pathArgKeys = []string{
	"remote_path",
	"from_path",
	"to_path",
	"link_path",
	"existing_path",
	"new_path",
	"target",
}

func forbiddenToolMessage(toolName string) string {
	return "tool " + toolName + " is not permitted on this session: it is an extension session whose capability list does not include it. Use extension(action=get) to see what this session was created to allow."
}

func forbiddenPathMessage(key, path string) string {
	return "path argument " + key + " is outside the directories this extension session may reach: " + path + ". Use extension(action=get) to see the allowed paths."
}
