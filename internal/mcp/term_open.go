package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

// pipe and pty — the two primitives, as directives.
//
// The mode is the directive, not a parameter: there is no `--mode` flag, no
// default, no `open`. The reason is that the difference is a TTY, and a TTY
// cannot be added after the fact. Choosing wrong is unfixable: a channel that
// should have been a pty and was made a pipe can never run a TUI or a password
// prompt, and one that should have been a pipe and was made a pty has echo and
// control sequences permanently mixed into its read cursor. Making the caller say
// which one it means is the only honest option.
//
// One primitive covers both cases the old surface split in two: a target that is
// an ENTRY starts a connection plus its first channel, and a target that is a
// SESSION HANDLE adds a channel to the connection that already exists. That is
// exactly what session_start and shell_open did; this does not change the
// behaviour, it changes how you ask for it.

// directiveOpen implements `pipe <target>` and `pty <target>`.
func (s *Server) directiveOpen(ctx context.Context, stmt directive.Statement, win termArgs, pty bool) []envelope.Slot {
	mode := "pipe"
	if pty {
		mode = "pty"
	}
	if stmt.Target == "internal" && s.NoInternal {
		return []envelope.Slot{{Code: 409, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConflict},
			Body: "the internal profile is disabled on this instance (it was started with --no-internal)"}}
	}
	capID := capability.ShellOpen
	if _, bad := s.requireSession(stmt.Target); bad != nil {
		// The target is not an existing session, so this call creates one.
		capID = capability.SessionStart
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return []envelope.Slot{slot}
	}

	argv, _ := stmt.Flag("argv")
	args := argsOf("mode", mode)
	if argv != "" {
		cmd, argvParts, err := directive.SplitArgv(argv)
		if err != nil {
			usage := ""
			if op, ok := directive.Lookup(stmt.Op); ok {
				usage = "\n" + op.Usage
			}
			return []envelope.Slot{{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
				Body: err.Error() + usage}}
		}
		if cmd == "" {
			return []envelope.Slot{{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
				Body: "--argv needs a command, e.g. pipe internal --argv sleep 5"}}
		}
		args["command"] = cmd
		if len(argvParts) > 0 {
			list := make([]any, 0, len(argvParts))
			for _, p := range argvParts {
				list = append(list, p)
			}
			args["args"] = list
		}
	}
	if name, ok := stmt.Flag("name"); ok {
		args["name"] = name
	}
	if onExit, ok := stmt.Flag("on-exit"); ok {
		args["on_exit"] = onExit
	}
	if size, ok := stmt.Flag("size"); ok {
		rows, cols := parseSize(size)
		if rows > 0 {
			args["rows"] = float64(rows)
		}
		if cols > 0 {
			args["cols"] = float64(cols)
		}
	}

	// An existing session takes a new channel on its connection; anything else
	// starts a connection. The distinction is the caller's target, not a flag.
	var res *mcpgo.CallToolResult
	var fail *toolFailure
	if capID == capability.SessionStart {
		args["ssh_config"] = stmt.Target
		res, fail = s.callTool(ctx, "session_start", args)
	} else {
		args["session_id"] = stmt.Target
		res, fail = s.callTool(ctx, "shell_open", args)
	}
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, fail)}
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})}
	}
	headers := map[string]string{"Path": stmt.Path}
	// The handle is NEWS here — the whole reason this call exists — so it is the
	// one reply that always carries it.
	headers["Handle"] = shortHandle(doc)
	return []envelope.Slot{{Code: 200, Headers: headers, Body: createdBody(doc, mode)}}
}

// createdBody renders the two facts a caller needs about a new channel: its
// handle and whether stdout/stderr are separable. The latter is news for a pipe
// and false for a pty, so it is stated once, here, instead of being learned by
// reading a stream that never separates them.
func createdBody(doc map[string]any, mode string) string {
	handle := shortHandle(doc)
	if mode == "pty" {
		return fmt.Sprintf("%s pty\n", handle)
	}
	return fmt.Sprintf("%s pipe (stderr separable: --stream e)\n", handle)
}

// shortHandle renders `<session>:N` from a handler reply.
func shortHandle(doc map[string]any) string {
	shellID := getString(doc, "shell_id", "")
	sessionID := getString(doc, "session_id", "")
	index := int(getFloatAny(doc, "index", 1))
	if sessionID != "" {
		return handleOf(sessionID, index)
	}
	return shellID
}

// parseSize reads the `--size 24x80` form: rows x cols, the order a terminal
// reports them.
func parseSize(raw string) (rows, cols int) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(raw)), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	fmt.Sscanf(parts[0], "%d", &rows)
	fmt.Sscanf(parts[1], "%d", &cols)
	return rows, cols
}

// alias, and the command space.
//
// An alias is a name for a directive: it is expanded ONCE, at parse time, and the
// reply's Path shows the expansion, so a caller reading the reply sees the command
// that actually ran rather than the name it typed. Runtime aliases are a draft —
// they live in this process and are gone at restart. Persisting one means writing
// it into an extension's [alias] table, which is the distribution unit: install the
// extension, get the command.

// runtimeAliases is this process's alias table.
//
// The lock is NOT decoration: a Term call is allowed to run concurrently with
// another (the MCP server hands each request to its own goroutine, and HTTP does
// the same), so an `alias jira …` in flight races every statement being parsed
// against a plain map. That is a data race the Go runtime panics on under -race and
// corrupts under load — the worst kind, because it shows up as an alias resolving
// to a mangled expansion rather than as a crash.
//
// ponytail: one RWMutex over one map. An alias table holds a handful of entries and
// is read once per statement, so sharding it would be code that never runs in the
// hot path it was written for.
type aliasTable struct {
	mu sync.RWMutex
	m  map[string]string
}

// aliases is the process-wide table.
var aliases = &aliasTable{m: map[string]string{}}

// get reads one entry.
func (a *aliasTable) get(name string) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	v, ok := a.m[name]
	return v, ok
}

// set writes one entry.
func (a *aliasTable) set(name, expansion string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m[name] = expansion
}

// drop removes one entry.
func (a *aliasTable) drop(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.m, name)
}

// snapshot copies the table for a listing, so the reply is rendered from one
// consistent moment instead of iterating a map another goroutine may be editing.
func (a *aliasTable) snapshot() map[string]string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]string, len(a.m))
	for k, v := range a.m {
		out[k] = v
	}
	return out
}

// Expand resolves a directive's first word through the alias table. The
// expansion may itself name an alias, so resolution repeats until it reaches a
// real directive (with a cycle guard).
func (a *aliasTable) Expand(stmt directive.Statement) directive.Statement {
	if _, ok := a.get(stmt.Op); !ok {
		return stmt
	}
	if resolved, ok := a.resolve(stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload)); ok {
		return resolved
	}
	return stmt
}

// expandError resolves a statement the parser could not parse against the alias
// table: an alias is a directive the caller invented, so the parser is right to
// refuse it and we are right to run it.
func (a *aliasTable) expandError(err error) (directive.Statement, bool) {
	derr, ok := err.(*directive.Error)
	if !ok || derr.Op == "" {
		return directive.Statement{}, false
	}
	return a.resolve(derr.Op, derr.Rest)
}

// resolve follows a name through the table until it lands on a directive the
// parser knows, or fails (unknown name, unparseable expansion, or a cycle).
func (a *aliasTable) resolve(name, rest string) (directive.Statement, bool) {
	seen := map[string]bool{}
	for {
		expansion, ok := a.get(name)
		if !ok || seen[name] {
			return directive.Statement{}, false
		}
		seen[name] = true
		f, err := directive.Parse(strings.TrimSpace(expansion + " " + rest))
		if err == nil && len(f.Statements) == 1 {
			stmt := f.Statements[0]
			if _, next := a.get(stmt.Op); !next {
				return stmt, true
			}
			name, rest = stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload)
			continue
		}
		derr, ok := err.(*directive.Error)
		if !ok || derr.Op == "" {
			return directive.Statement{}, false
		}
		name, rest = derr.Op, derr.Rest
	}
}

// directiveAlias manages the runtime table.
func (s *Server) directiveAlias(_ context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}
	// The payload is "<name> <expansion...>" verbatim, so the expansion is never
	// re-quoted: what the caller writes is what the alias runs.
	body := strings.TrimSpace(stmt.Payload)
	if body == "" {
		entries := aliases.snapshot()
		if len(entries) == 0 {
			return envelope.Slot{Code: 200, Headers: headers, Body: "(no aliases)\n"}
		}
		var b strings.Builder
		for name, expansion := range entries {
			b.WriteString(name + " = " + expansion + "\n")
		}
		capped, code := capGenerated(b.String(), win)
		return envelope.Slot{Code: code, Headers: headers, Body: capped}
	}
	name, expansion, _ := strings.Cut(body, " ")
	name, expansion = strings.TrimSpace(name), strings.TrimSpace(expansion)
	if expansion == "--off" {
		aliases.drop(name)
		return envelope.Slot{Code: 204, Headers: headers}
	}
	if expansion == "" {
		return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "invalid_argument"},
			Body: "alias needs a directive to expand to: alias jira mcp open https://..."}
	}
	aliases.set(name, expansion)
	return envelope.Slot{Code: 204, Headers: headers}
}
