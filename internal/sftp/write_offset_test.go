package sftp

import (
	"os"
	"testing"
)

// TestWriteOpenFlagsDistinguishesZeroFromTruncate pins the fix for bug #1: writing at
// offset 0 must NOT truncate the file, while omitting offset (-1) must truncate.
func TestWriteOpenFlagsDistinguishesZeroFromTruncate(t *testing.T) {
	// Negative (-1): truncate, reset offset to 0.
	flag, off := writeOpenFlags(-1)
	if flag&os.O_TRUNC == 0 {
		t.Errorf("offset -1 must carry O_TRUNC, got flag %x", flag)
	}
	if off != 0 {
		t.Errorf("normalized offset for -1 = %d, want 0", off)
	}

	// Zero: must NOT truncate; keep offset at 0.
	flag, off = writeOpenFlags(0)
	if flag&os.O_TRUNC != 0 {
		t.Errorf("offset 0 must NOT carry O_TRUNC, got flag %x", flag)
	}
	if off != 0 {
		t.Errorf("offset 0 should stay 0, got %d", off)
	}

	// Positive: must NOT truncate; keep offset as given.
	flag, off = writeOpenFlags(42)
	if flag&os.O_TRUNC != 0 {
		t.Errorf("offset 42 must NOT carry O_TRUNC, got flag %x", flag)
	}
	if off != 42 {
		t.Errorf("offset 42 should stay 42, got %d", off)
	}
}
