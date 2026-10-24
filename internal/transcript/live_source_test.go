package transcript

import (
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

// TestALiveSourceIsNeverShorterThanItsLog pins the two records of one stream to one
// answer.
//
// A live shell's bytes are written TWICE, and in this order on purpose: the durable log
// first (crash safety), the in-memory buffer second (positioned by the offset the log
// returned). Readers are split across the two — `--from-line` and `--grep` read the log,
// `--tail` and `--offset` read the buffer — so between those two writes the same channel
// answered two different lengths. With the gap widened to 1.5s on Linux:
//
//	read <ch> --from-line 1    200, the bytes
//	read <ch> --tail 20        204, Content-Range: bytes */0
//
// `bytes */0` is a false fact about a channel that has them, and it is what made a GBK
// test fail on one CI runner and pass on another.
//
// The source here is a live shell whose buffer is BEHIND its log, which is exactly the
// state the write order produces for a few microseconds per chunk. The buffer is empty
// rather than short because a ChildShell with no buffer reports length 0 — the same
// answer the runner saw.
func TestALiveSourceIsNeverShorterThanItsLog(t *testing.T) {
	store := storage.New(t.TempDir())
	msgMgr := message.NewManager(store)
	t.Cleanup(func() {
		// The log file stays open under the manager, and Windows refuses to remove a
		// directory holding an open file — so the cleanup is part of the fixture.
		msgMgr.ForgetSession("s1")
		_ = store.Close()
	})

	const sessID, shellID = "s1", "sh1"
	payload := []byte{0xD2, 0xD4, 0xCF, 0xC2, 0x0A} // 以下 in GBK, plus a newline
	if _, err := msgMgr.AppendOutput(sessID, shellID, payload); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The live record: a shell whose buffer has not been written yet.
	src := &Source{Live: &session.ChildShell{}, SessID: sessID, ShellID: shellID, MsgMgr: msgMgr}

	if got := src.Live.BufferLen(); got != 0 {
		t.Fatalf("this test is about a buffer that is BEHIND the log; it holds %d bytes", got)
	}
	n, err := src.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if n != int64(len(payload)) {
		t.Errorf("Len = %d, want %d: the log is the record and the buffer is a cache, so the length cannot be the cache's", n, len(payload))
	}
	// And the bytes themselves, not only the count: a length without its bytes is a
	// range the caller cannot read.
	got, total, err := src.ByteRange(0, len(payload))
	if err != nil {
		t.Fatalf("ByteRange: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("ByteRange = %x, want %x", got, payload)
	}
	if total != int64(len(payload)) {
		t.Errorf("ByteRange total = %d, want %d", total, len(payload))
	}
}
