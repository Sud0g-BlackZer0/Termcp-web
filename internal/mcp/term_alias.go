package mcp

import (
	"context"
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
)

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
	// NO SUBSTITUTION HERE: this runs on an already-parsed statement, so the
	// caller's values have been filled in. Passing an empty table keeps the
	// expansion's own `$N` text as the shell's own positional parameters, which is
	// what it was before this path existed.
	if resolved, ok := a.resolve(stmt.Op, strings.TrimSpace(stmt.Target+" "+stmt.Payload), directive.Subst{}); ok {
		return resolved
	}
	return stmt
}

// expandError resolves a statement the parser could not parse against the alias
// table: an alias is a directive the caller invented, so the parser is right to
// refuse it and we are right to run it.
//
// NO SUBSTITUTION TABLE IS PASSED ON, and that is deliberate. `derr.Rest` is the
// caller's text AFTER the first parse already filled it in — the `$1` in `ech $1`
// became the caller's value before this function ran. Re-applying the same table
// would fill the ALIAS'S OWN `$1` with that same value a second time, so
// `alias ech run h $1` used as `ech $1` sent the argument twice:
//
//	run h:1 'echo hello' 'echo hello'
//
// The two `$` namespaces belong to different authors. A caller's `$N` is the
// caller's value; an alias's `$N` is whatever the alias author meant, which this
// server cannot know and must not guess. Leaving it alone hands it to the shell.
func (a *aliasTable) expandError(err error) (directive.Statement, bool) {
	derr, ok := err.(*directive.Error)
	if !ok || derr.Op == "" {
		return directive.Statement{}, false
	}
	return a.resolve(derr.Op, derr.Rest, directive.Subst{})
}

// resolve follows a name through the table until it lands on a directive the
// parser knows, or fails (unknown name, unparseable expansion, or a cycle).
//
// IT RE-PARSES WITH THE CALLER'S SUBSTITUTION TABLE, and that is not a detail. The
// first parse happened against `ech $1`, where `ech` was an unknown directive — so
// the `$1` was still raw text. If this pass parsed the expansion WITHOUT the table,
// the value would never be filled in; if it parsed the ALREADY-SUBSTITUTED rest, the
// value would arrive quoted as data and a positional payload would stop being syntax.
// Passing the table through is what makes an alias expand to the same statement the
// caller would have written out in full.
func (a *aliasTable) resolve(name, rest string, sub directive.Subst) (directive.Statement, bool) {
	seen := map[string]bool{}
	for {
		expansion, ok := a.get(name)
		if !ok || seen[name] {
			return directive.Statement{}, false
		}
		seen[name] = true
		f, err := directive.ParseWithSubst(strings.TrimSpace(expansion+" "+rest), sub)
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
		return badRequest(stmt, "alias needs a directive to expand to: alias jira mcp open https://...")
	}
	aliases.set(name, expansion)
	return envelope.Slot{Code: 204, Headers: headers}
}
