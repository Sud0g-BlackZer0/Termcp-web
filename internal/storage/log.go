package storage

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// AppendLog appends raw bytes to a shell's log.bin and returns the offset the
// data was written at.
//
// The returned offset is the file position, which is what makes an offset
// meaningful without any bookkeeping: it is a fact about the file, not a sum of
// lengths that can drift out of agreement with the bytes.
//
// The append happens through a cached handle so a burst of small writes does not
// pay an open() per write. Callers must AppendMark *after* this returns so a
// crash can only ever leave a trailing span with no mark (attributed to the
// previous status), never a mark pointing past the end of the data.
func (s *Store) AppendLog(sessionID, shellID string, data []byte) (int64, error) {
	if err := validateID(sessionID); err != nil {
		return 0, err
	}
	if err := validateID(shellID); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return s.LogSize(sessionID, shellID)
	}
	return s.logSink.append(sessionID, shellID, data)
}

// AppendMark appends one status transition to a shell's log.jsonl.
//
// Append this after the corresponding AppendLog: the mark then always points at
// a byte that exists. The reverse order would produce marks pointing past EOF,
// which cannot be repaired.
func (s *Store) AppendMark(sessionID, shellID string, mark api.LogMark) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(shellID); err != nil {
		return err
	}
	dir := s.shellDir(sessionID, shellID)
	if err := s.initLogDir(sessionID, shellID); err != nil {
		return err
	}
	line, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	// Marks are a handful of bytes per status change, so open/append/close keeps
	// their durability story simple and never holds a second handle open.
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// LogSize returns the current size of a shell's log.bin, i.e. the offset just
// past the last byte ever written.
func (s *Store) LogSize(sessionID, shellID string) (int64, error) {
	st, err := os.Stat(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size(), nil
}

// --- line index (log.lines) -------------------------------------------------
//
// log.lines is a sidecar index over log.bin, one shell each, maintained by the
// same sink that appends the bytes. It is a flat array of fixed-width records:
// record N-1 (8-byte little-endian) is the absolute offset of the '\n' that
// terminates line N. Line numbers are therefore absolute and 1-based, and they
// never shift: log.bin is append-only, so line N is the line that was N when it
// was written, for the life of the shell. A trailing partial line (bytes after
// the last '\n') has no record and no line number, matching how shell_output
// reports it.
//
// The index is advisory, like the mark index: a missing, stale or torn log.lines
// never changes what bytes exist, only how fast a line can be found. Every
// mutation goes through the sink, so appends and catch-up scans cannot
// interleave; readers use positional reads and never trust a torn tail record.

// lineRecordSize is the width of one log.lines record.
const lineRecordSize = 8

// LineIndexPath returns the path of a shell's line index.
func (s *Store) LineIndexPath(sessionID, shellID string) string {
	return filepath.Join(s.shellDir(sessionID, shellID), "log.lines")
}

// LineRecordCount returns how many complete lines the index holds: the file size
// in whole records. A torn tail record (a crash mid-append) is not a line and is
// not counted; the next append through the sink truncates it away.
func (s *Store) LineRecordCount(sessionID, shellID string) (int64, error) {
	if err := validateID(sessionID); err != nil {
		return 0, err
	}
	if err := validateID(shellID); err != nil {
		return 0, err
	}
	st, err := os.Stat(s.LineIndexPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size() / lineRecordSize, nil
}

// ReadLineRecords returns count records starting at record first (0-based). A
// range past the end returns fewer records, like a short file read.
func (s *Store) ReadLineRecords(sessionID, shellID string, first int64, count int) ([]int64, error) {
	if err := validateID(sessionID); err != nil {
		return nil, err
	}
	if err := validateID(shellID); err != nil {
		return nil, err
	}
	if first < 0 || count <= 0 {
		return nil, nil
	}
	f, err := os.Open(s.LineIndexPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, int64(count)*lineRecordSize)
	n, err := f.ReadAt(buf, first*lineRecordSize)
	if err != nil && err != io.EOF {
		return nil, err
	}
	n -= n % lineRecordSize
	out := make([]int64, 0, n/lineRecordSize)
	for i := 0; i+lineRecordSize <= n; i += lineRecordSize {
		out = append(out, int64(binary.LittleEndian.Uint64(buf[i:])))
	}
	return out, nil
}

// SyncLineIndex brings a shell's line index up to date with its log.bin: it
// scans the bytes after the last indexed '\n' and appends a record for each
// newline found. It is the repair path for an index that is missing (a session
// that predates log.lines), lagging (a crash between the byte append and the
// record append), or torn (a truncated final record).
//
// The work is a sink request, so it serializes against every append: a scan
// cannot race the writer into appending a record twice. Idempotent by
// construction - it always resumes from the last indexed newline, re-deriving
// that position from the files rather than trusting memory.
func (s *Store) SyncLineIndex(sessionID, shellID string) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(shellID); err != nil {
		return err
	}
	return s.logSink.do(sinkRequest{op: opSyncLines, session: sessionID, shell: shellID}).err
}

// ReadLog reads at most max bytes of a shell's log.bin starting at offset.
//
// This is a positional read: it never consults log.jsonl and never accumulates
// lengths, so a window is exactly the file's bytes at that position.
func (s *Store) ReadLog(sessionID, shellID string, offset int64, max int) ([]byte, error) {
	if offset < 0 {
		offset = 0
	}
	if max <= 0 {
		return nil, nil
	}
	f, err := os.Open(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, max)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}
