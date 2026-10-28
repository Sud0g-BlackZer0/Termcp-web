package mcp

import (
	"path/filepath"
	"strings"
	"testing"
)

// A FLAG THAT CANNOT BE HONOURED IS REFUSED, NOT DROPPED (#190).
//
// This file used to hold three tests about `--local-path`: that an illegal `--mode`
// was refused on it exactly as on the `--data` route, that a decoding mode said it did
// not apply rather than pretending to, and that leaving `--mode` off stayed accepted.
// All three were about a flag that no longer exists, so they are replaced by the one
// test the removal needs.
func TestLocalPathIsRetiredAndRefused(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)

	got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file /tmp/dst.sh --local-path /tmp/src.sh",
	})
	if !statusLineIs(got, "400") {
		t.Fatalf("--local-path must be refused rather than accepted and ignored:\n%s", got)
	}
	if !strings.Contains(got, "is gone") {
		t.Errorf("the refusal must say the flag is gone:\n%s", got)
	}
	if !strings.Contains(got, "pipeline") {
		t.Errorf("the refusal must name the route that replaced it:\n%s", got)
	}
}

// THE REASON IT WAS AN HOLE AND NOT ONLY A DUPLICATE.
//
// --no-internal hides the loopback profile so an MCP client cannot reach the machine
// termcp runs on. --local-path read that machine's filesystem from inside the file
// handler, with no capability check and no reference to the profile — so the isolation
// flag was decorative: the client could still name any path and get its contents
// streamed to a session it controlled, or write its bytes into a file it chose.
//
// This test fails if the route comes back, and it is written against the parser rather
// than the handler because the parser is where the refusal has to happen: a handler
// that still accepted local_path would be reachable again the moment anything but the
// DSL called it.
func TestLocalPathCannotBeUsedToReachTheDaemonFilesystem(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)

	// A file the daemon can certainly read, named the way an attacker would name it.
	for _, cmd := range []string{
		"write " + sid + " file /tmp/exfil.bin --local-path /etc/hosts",
		"write " + sid + " file /tmp/exfil.bin --local-path=/etc/hosts",
	} {
		got := termText(t, s, map[string]any{"cmd": cmd})
		if !statusLineIs(got, "400") {
			t.Errorf("%q reached the handler instead of being refused:\n%s", cmd, got)
		}
	}
}

// STAT HAS TO ANSWER "IS THIS EXECUTABLE" (#191). A script that had just been written
// failed to run, and every surface that could have said "mode 664" reported size and
// mtime instead: the caller went looking at mounts, users and paths while the fact sat
// in the stat response nobody had asked for.
func TestStatReportsTheModeOfAFile(t *testing.T) {
	s := newTestServer(t)
	sid := internalSession(t, s)
	// THE PATH IS THE TEST'S OWN TEMP DIR, not /tmp. On a Windows runner /tmp resolves to
	// D:\tmp, which does not exist, so the write was refused with "the directory /tmp does
	// not exist" — a green local suite (this machine has C:\tmp) and a red windows leg.
	// t.TempDir is the repo's rule for exactly this reason: never hardcode a shared
	// absolute path.
	path := filepath.Join(t.TempDir(), "stat-mode-probe.sh")
	if got := termText(t, s, map[string]any{
		"cmd": "write " + sid + " file " + path + " --data hi",
	}); !statusLineIs(got, "200") {
		t.Fatalf("write:\n%s", got)
	}
	defer termText(t, s, map[string]any{"cmd": "rm " + sid + " file " + path})

	got := termText(t, s, map[string]any{
		"cmd": "get " + sid + " file " + path + " --op stat",
	})
	if !statusLineIs(got, "200") {
		t.Fatalf("stat:\n%s", got)
	}
	// Four octal digits, e.g. "0644": the spelling a caller passes to chmod.
	if !strings.Contains(got, "mode: 0") {
		t.Fatalf("stat did not report the file's mode:\n%s", got)
	}
}
