package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileFindRecursesAndReturnsStructuredHits pins feedback §2.2: the file search
// primitive. It must return structured entries rather than text the caller has to
// parse, and must support glob patterns and directory exclusions.
func TestFileFindRecursesAndReturnsStructuredHits(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	sessID := strings.Split(h, ":")[0]

	// Fixture tree:
	// root/
	//   a.go
	//   b.txt
	//   sub/
	//     c.go
	//   node_modules/
	//     d.go  (should be skipped by default)
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.go"), []byte("package a"), 0644)
	_ = os.WriteFile(filepath.Join(root, "b.txt"), []byte("hello"), 0644)
	_ = os.MkdirAll(filepath.Join(root, "sub"), 0755)
	_ = os.WriteFile(filepath.Join(root, "sub", "c.go"), []byte("package c"), 0644)
	_ = os.MkdirAll(filepath.Join(root, "node_modules"), 0755)
	_ = os.WriteFile(filepath.Join(root, "node_modules", "d.go"), []byte("package d"), 0644)

	// 1. Find all *.go files. node_modules must be excluded by default.
	got := termText(t, s, map[string]any{"cmd": "file " + sessID +
		" --action find --remote-path " + root + " --pattern '*.go'"})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("find failed:\n%s", got)
	}

	body := got[strings.Index(got, "\n\n")+2:]
	_ = json.Unmarshal // the envelope renders structured JSON, checked below by key

	// The reply carries the fields the caller needs, by name.
	for _, want := range []string{"entries:", "total:", "truncated:"} {
		if !strings.Contains(body, want) {
			t.Errorf("find reply is missing %q:\n%s", want, body)
		}
	}

	// Must find a.go and c.go, but NEVER d.go (inside node_modules).
	for _, want := range []string{"a.go", "sub/c.go"} {
		if !strings.Contains(body, want) {
			t.Errorf("find results are missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "d.go") {
		t.Errorf("node_modules was not skipped; found d.go:\n%s", body)
	}
	if strings.Contains(body, "b.txt") {
		t.Errorf("pattern *.go should have excluded b.txt:\n%s", body)
	}
	if !strings.Contains(body, "node_modules") {
		t.Errorf("the reply should name the directory it skipped:\n%s", body)
	}

	// 2. Limit and truncation.
	gotLimited := termText(t, s, map[string]any{"cmd": "file " + sessID +
		" --action find --remote-path " + root + " --limit 1"})
	if !strings.Contains(gotLimited, "truncated: true") {
		t.Errorf("expected truncated: true when limit is hit:\n%s", gotLimited)
	}
}
