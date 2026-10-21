package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/sftp"
)

// TestFileResultZeroLengthKeepsShape pins bug #11: a zero-length read must still
// report data, length and bytes_read rather than omitting them under omitempty.
func TestFileResultZeroLengthKeepsShape(t *testing.T) {
	empty := ""
	r := &sftp.FileResult{
		Mode:      "text",
		TotalSize: 10,
		Offset:    10,
		Length:    0,
		BytesRead: 0,
		Data:      &empty,
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"data", "length", "bytes_read", "mode", "total_size"} {
		if _, ok := m[want]; !ok {
			t.Errorf("zero-length FileResult JSON must contain %q, got: %s", want, string(b))
		}
	}
}

// TestInternalRealpathResolvesSymlink pins bug #7: realpath on the internal profile
// must resolve symlinks through to their real target, like a real SSH host does.
func TestInternalRealpathResolvesSymlink(t *testing.T) {
	s := newTestServer(t)
	text := termText(t, s, map[string]any{"cmd": "pty internal --name live"})
	h := handleFrom(t, text)
	sid := strings.SplitN(h, ":", 2)[0]

	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got := termText(t, s, map[string]any{
		"cmd": "file " + sid + " --action fs --op realpath --remote-path " + link,
	})
	// Normalise path separators before checking: the SFTP server emits wire-shaped
	// POSIX paths ("/C:/dir/target.txt" on Windows), while filepath.Join emits the
	// platform shape ("C:\dir\target.txt"). Compare the normalized target basename
	// and path rather than raw platform bytes.
	want := filepath.ToSlash(target)
	gotSlash := filepath.ToSlash(got)
	if !strings.Contains(gotSlash, want) && !strings.Contains(gotSlash, "/"+want) {
		t.Errorf("realpath of symlink must resolve to target %q, got:\n%s", target, got)
	}
}
