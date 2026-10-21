package sftp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/clock"
)

// ErrIsDirectory reports that a read named a directory.
//
// A sentinel rather than a formatted string because the MCP layer classifies it: a
// directory is what the caller asked for and cannot use, which is a 400, while an
// SSH_FX_FAILURE from the same reader is a genuine fault and stays a 500.
var ErrIsDirectory = errors.New("is a directory")

// ReplaceInFile errors: exact matching only.
var (
	ErrPatternNotFound  = errors.New("pattern not found in file")
	ErrPatternAmbiguous = errors.New("pattern matched more than once; must match uniquely")
)

// FileResult holds the result of a file_read operation.
type FileResult struct {
	// Data is a POINTER so "the inline body is empty" and "this read has no inline
	// body" stay distinguishable.
	//
	// mode=file streams to a local path and never fills Data; mode=text/hex always
	// does, even when the window is empty. A plain string with no omitempty made the
	// first case report `data: ""` — a body that looks truncated rather than absent —
	// while keeping omitempty made the second case drop the field on a zero-length
	// read, which is the one read where seeing a zero matters.
	//
	// Length and BytesRead keep no omitempty for the same reason they always needed
	// it: 0 is a fact about the window, not a missing value.
	Data      *string `json:"data,omitempty"`
	Mode      string  `json:"mode"` // "text" | "hex" | "file"
	Encoding  string  `json:"encoding,omitempty"`
	TotalSize int64   `json:"total_size"`
	HasMore   bool    `json:"has_more,omitempty"`
	Offset    int64   `json:"offset,omitempty"`
	Length    int64   `json:"length"`
	BytesRead int64   `json:"bytes_read"`
	LocalPath string  `json:"local_path,omitempty"`
}

// FileStatResult holds the result of a file_stat operation.
type FileStatResult struct {
	Name     string           `json:"name"`
	Size     int64            `json:"size"`
	IsDir    bool             `json:"is_dir"`
	ModTime  int64            `json:"mod_time,omitempty"` // Unix ms; 0 = unknown
	Children []FileStatResult `json:"children,omitempty"`
}

// sftpClient abstracts SFTP operations.
type Client struct {
	client *sftp.Client
}

// Close closes the SFTP client.
func (s *Client) Close() error {
	return s.client.Close()
}

// RemoveFile deletes a remote file.
func (s *Client) RemoveFile(remotePath string) error {
	return s.client.Remove(remotePath)
}

// RenameFile moves or renames a remote file/directory (same filesystem).
func (s *Client) RenameFile(oldPath, newPath string) error {
	return s.client.Rename(oldPath, newPath)
}

// MakeDir creates a directory (and parents) on the remote.
func (s *Client) MakeDir(remotePath string) error {
	return s.client.MkdirAll(remotePath)
}

// StatFile returns file/directory info.
func (s *Client) StatFile(remotePath string) (*FileStatResult, error) {
	fi, err := s.client.Stat(remotePath)
	if err != nil {
		return nil, fmt.Errorf("stat: %w", err)
	}

	result := &FileStatResult{
		Name:    filepath.Base(remotePath),
		Size:    fi.Size(),
		IsDir:   fi.IsDir(),
		ModTime: clock.Millis(fi.ModTime()),
	}

	if fi.IsDir() {
		entries, err := s.client.ReadDir(remotePath)
		if err != nil {
			return result, nil // partial result
		}
		for _, e := range entries {
			result.Children = append(result.Children, FileStatResult{
				Name:    e.Name(),
				Size:    e.Size(),
				IsDir:   e.IsDir(),
				ModTime: clock.Millis(e.ModTime()),
			})
		}
	}

	return result, nil
}

// ChmodFile changes the permissions of a remote file.
func (s *Client) ChmodFile(remotePath string, mode os.FileMode) error {
	return s.client.Chmod(remotePath, mode)
}

// ChownFile changes the owner and group of a remote file.
func (s *Client) ChownFile(remotePath string, uid, gid int) error {
	return s.client.Chown(remotePath, uid, gid)
}

// ChtimesFile changes the access and modification times of a remote file.
func (s *Client) ChtimesFile(remotePath string, atime, mtime time.Time) error {
	return s.client.Chtimes(remotePath, atime, mtime)
}

// ReadLink returns the target of a symbolic link.
func (s *Client) ReadLink(path string) (string, error) {
	return s.client.ReadLink(path)
}

// SymlinkFile creates a symbolic link on the remote.
// target is the existing path, linkPath is the new symlink to create.
func (s *Client) SymlinkFile(target, linkPath string) error {
	return s.client.Symlink(target, linkPath)
}

// LinkFile creates a hard link on the remote.
func (s *Client) LinkFile(existing, newPath string) error {
	return s.client.Link(existing, newPath)
}

// TruncateFile truncates a remote file to the given size.
func (s *Client) TruncateFile(path string, size int64) error {
	return s.client.Truncate(path, size)
}

// RealPath returns the canonical absolute path of a remote file or directory.
func (s *Client) RealPath(path string) (string, error) {
	return s.client.RealPath(path)
}

// FsStatResult holds the result of a file_statvfs operation.
type FsStatResult struct {
	TotalSpace  uint64 `json:"total_space"`
	FreeSpace   uint64 `json:"free_space"`
	AvailSpace  uint64 `json:"avail_space"`
	TotalINodes uint64 `json:"total_inodes"`
	FreeINodes  uint64 `json:"free_inodes"`
	AvailINodes uint64 `json:"avail_inodes"`
	BlockSize   uint64 `json:"block_size"`
}

// fsStatFromVFS converts a pkg/sftp StatVFS to FsStatResult.
func fsStatFromVFS(v *sftp.StatVFS) *FsStatResult {
	return &FsStatResult{
		TotalSpace:  v.Frsize * v.Blocks,
		FreeSpace:   v.Frsize * v.Bfree,
		AvailSpace:  v.Frsize * v.Bavail,
		TotalINodes: v.Files,
		FreeINodes:  v.Ffree,
		AvailINodes: v.Favail,
		BlockSize:   v.Frsize,
	}
}

// StatVFS returns filesystem statistics for the given remote path.
func (s *Client) StatVFS(path string) (*FsStatResult, error) {
	v, err := s.client.StatVFS(path)
	if err != nil {
		return nil, fmt.Errorf("statvfs: %w", err)
	}
	return fsStatFromVFS(v), nil
}

// Getwd returns the remote working directory.
func (s *Client) Getwd() (string, error) {
	return s.client.Getwd()
}

// OpenSFTPOverSSH opens an SFTP connection over an existing SSH client (for regular SSH).
func NewClient(sshClient *ssh.Client) (*Client, error) {
	if sshClient == nil {
		return nil, fmt.Errorf("ssh client is nil")
	}
	sftpCli, err := sftp.NewClient(sshClient)
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return &Client{client: sftpCli}, nil
}

// FindEntry is one hit from FindFiles.
type FindEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	IsDir   bool   `json:"is_dir"`
	ModTime int64  `json:"mod_time,omitempty"`
}

// FindResult is a walk with its own accounting: a caller must be able to tell a
// complete answer from a truncated one.
type FindResult struct {
	Entries   []FindEntry `json:"entries"`
	Total     int         `json:"total"`
	Truncated bool        `json:"truncated"`
	// SkippedDirectories names the directories excluded by rule, so a caller that
	// does not find what it wants knows where the search did not look.
	SkippedDirectories []string `json:"skipped_directories,omitempty"`
}

// FindFiles walks a remote tree and returns entries whose base name matches
// pattern (a shell glob; "" matches everything).
//
// SFTP, NOT A REMOTE `find`. Every sshd that Termcp can connect to serves SFTP —
// the same channel file read, write and replace already use — while `find`, `grep`
// and `rg` are absent on Windows targets, missing from distroless images, and
// differ between GNU and busybox. A search that silently returns nothing because the
// target has no `find` is the worst outcome: the caller concludes the file is not
// there. Walking the tree ourselves costs round trips but behaves identically
// everywhere.
//
// exclude holds directory BASE names to skip. Without it, one search in a real
// repository spends its whole budget inside node_modules and .git and reports
// "truncated" without ever reaching the source — which is why the default set
// includes both.
//
// limit bounds the result, and truncated reports whether the walk stopped early so
// the caller knows to narrow the pattern rather than assume the tree ended.
func (s *Client) FindFiles(root, pattern string, exclude []string, limit int) (*FindResult, error) {
	if limit <= 0 {
		limit = defaultFindLimit
	}
	if limit > maxFindLimit {
		limit = maxFindLimit
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[e] = true
	}

	res := &FindResult{Entries: make([]FindEntry, 0, min(limit, 64))}
	skipped := make(map[string]bool)

	walker := s.client.Walk(root)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			// A single unreadable directory must not abort the whole search: report
			// what was found and let the caller decide. Only a failure on the ROOT
			// itself is fatal, because then nothing was searched at all.
			if walker.Path() == root {
				return nil, fmt.Errorf("walk %s: %w", root, err)
			}
			continue
		}
		path := walker.Path()
		info := walker.Stat()
		if info == nil {
			continue
		}
		base := filepath.Base(path)
		if info.IsDir() {
			if path != root && skip[base] {
				skipped[base] = true
				walker.SkipDir()
				continue
			}
			// The root itself is never a result: the caller named it.
			if path == root {
				continue
			}
		}
		if pattern != "" {
			ok, err := filepath.Match(pattern, base)
			if err != nil {
				return nil, fmt.Errorf("bad pattern %q: %w", pattern, err)
			}
			if !ok {
				continue
			}
		}
		if len(res.Entries) >= limit {
			res.Truncated = true
			break
		}
		res.Entries = append(res.Entries, FindEntry{
			Path:    path,
			Size:    info.Size(),
			IsDir:   info.IsDir(),
			ModTime: clock.Millis(info.ModTime()),
		})
	}
	res.Total = len(res.Entries)
	for name := range skipped {
		res.SkippedDirectories = append(res.SkippedDirectories, name)
	}
	sort.Strings(res.SkippedDirectories)
	return res, nil
}

const (
	defaultFindLimit = 200
	maxFindLimit     = 5000
)
