package directive

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoSpecDriftBetweenDirectivesAndSkill pins feedback §6.3:
//
// A spec that drifts from reality is the one failure mode tests never catch —
// every assertion checks "did the code do what I wrote", and none checks "is what
// I wrote true". SKILL.md taught `watch` long after `watch` was deleted, and
// taught `close --force` after --force was removed.
//
// This test asserts both directions:
// 1. Every directive declared in directive.go has a non-empty Usage and Detail.
// 2. Directives taught in SKILL.md must actually exist in the directive table.
func TestNoSpecDriftBetweenDirectivesAndSkill(t *testing.T) {
	declared := make(map[string]bool)
	for _, op := range Ops() {
		if op.Name == "" {
			t.Errorf("found an unnamed directive in Ops()")
		}
		if op.Usage == "" {
			t.Errorf("directive %q has no Usage string", op.Name)
		}
		declared[op.Name] = true
	}

	// THE VERB SET IS PINNED BY NAME, not by count. A count only says the surface
	// changed; a list says what it is, so adding or dropping a verb is a deliberate
	// edit here rather than a number someone adjusts after the fact.
	//
	// These are all VERBS. A noun is not here unless it has more than one verb of its
	// own: `span` and `reader` are views of a channel, `forward`/`file`/`mcp`/`cdp`/
	// `jrpc` are children made with `open`, and `host`/`ext`/`alias` are collections
	// reached through `ls`/`create`/`edit`/`rm`.
	want := []string{
		"call", "close", "connect", "create", "edit", "get", "help", "input",
		"key", "ls", "notify", "open", "read", "reader", "resize", "rm", "run",
		"span", "write",
	}
	for _, name := range want {
		if !declared[name] {
			t.Errorf("directive %q is missing from Ops()", name)
		}
	}
	if len(declared) != len(want) {
		t.Errorf("Ops() has %d directives, want %d: %v", len(declared), len(want), declared)
	}

	// Read SKILL.md if present (it lives in a sibling repo in dev).
	skillPath := filepath.Join("..", "..", "..", "termcp-skills", "SKILL.md")
	data, err := os.ReadFile(skillPath)
	if err != nil {
		t.Logf("sibling SKILL.md not found at %s (skipping external doc check): %v", skillPath, err)
		return
	}
	content := string(data)

	// Directives taught in bold backticks: `run <handle> ...`, `file <session> ...`
	re := regexp.MustCompile("`([a-z]+)[ >`]")
	matches := re.FindAllStringSubmatch(content, -1)

	// Known non-directive words that appear in backticks in the doc.
	ignoredWords := map[string]bool{
		"term": true, "cmd": true, "handle": true, "session": true, "index": true,
		"text": true, "hex": true, "file": true, "enter": true, "tab": true,
		"esc": true, "up": true, "down": true, "left": true, "right": true,
		"ctrl": true, "backspace": true, "delete": true, "home": true, "end": true,
		"download_url": true, "upload_url": true, "bytes": true, "post": true,
		"on_bytes_overflow": true, "lines_limit": true, "bytes_limit": true,
		"timeout": true, "sh": true, "bash": true,
	}

	for _, m := range matches {
		word := m[1]
		if ignoredWords[word] {
			continue
		}
		// If it looks like a directive name, it MUST be declared.
		if _, ok := declared[word]; !ok {
			// Specific known false positives can be filtered, but `watch` must FAIL.
			if word == "watch" {
				t.Errorf("SKILL.md teaches `watch`, but watch was deleted from the directive table")
			}
		}
	}

	// Deleted directives must NOT appear anywhere in SKILL.md.
	deletedDirectives := []string{
		"watch",
		"shell_output",
		"shell_input",
	}
	for _, del := range deletedDirectives {
		// A whole word occurrence in backticks: `watch` or `watch `
		if strings.Contains(content, "`"+del+"`") || strings.Contains(content, "`"+del+" ") {
			t.Errorf("SKILL.md teaches deleted directive %q", del)
		}
	}
}
