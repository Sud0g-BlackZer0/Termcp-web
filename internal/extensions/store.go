package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// ErrNotFound reports that no extension carries the name a caller asked for.
// Callers branch on it with errors.Is rather than matching a message.
var ErrNotFound = errors.New("extension not found")

// ErrExists reports a name collision, so a batch caller can present it as a
// per-name outcome without re-reading the store.
var ErrExists = errors.New("extension already exists")

// Store persists extensions as dataDir/extensions/<name>/config.toml, the same
// shape ssh_configs uses: one directory per name, written atomically, so a
// crash cannot leave a half-written template behind.
type Store struct {
	dataDir  string
	mu       sync.Mutex
	onChange atomic.Value // func(), fired after Save/Delete/Rename
}

// SetOnChange registers a callback fired after a template changes. The
// reconciler uses it to re-evaluate what should be warm.
func (s *Store) SetOnChange(fn func()) {
	s.onChange.Store(fn)
}

func (s *Store) notifyChange() {
	if fn, _ := s.onChange.Load().(func()); fn != nil {
		fn()
	}
}

// NewStore returns a Store rooted at dataDir, made absolute when that succeeds.
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	return &Store{dataDir: d}
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "extensions")
}

func (s *Store) configPath(name string) (string, error) {
	if err := sshconfig.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

// Load reads and validates one extension by name.
func (s *Store) Load(name string) (*Extension, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("extension %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	e, err := Parse(data)
	if err != nil {
		return nil, err
	}
	e.Name = name
	e.Normalize()
	if err := e.Validate(); err != nil {
		// A template that no longer validates is reported rather than silently
		// downgraded: the reconciler would otherwise keep failing to bring it
		// up with no indication of why.
		return nil, err
	}
	return e, nil
}

// ReadRaw returns the file bytes for an editor round trip.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("extension %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	if _, err := Parse(data); err != nil {
		return nil, err
	}
	return data, nil
}

// List returns the sorted names of stored extensions.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if st, err := os.Stat(filepath.Join(s.root(), e.Name(), "config.toml")); err == nil && !st.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save writes a template under its own name, creating the directory first.
// The name is the directory, so renaming means a move.
func (s *Store) Save(e *Extension) error {
	e.Normalize()
	if err := e.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.configPath(e.Name)
	if err != nil {
		return err
	}
	data, err := toml.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal extension: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// Delete removes an extension and its directory. A missing one is not an error:
// the caller's intent — this name is gone — already holds.
func (s *Store) Delete(name string) error {
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := retryFS(func() error { return os.Remove(p) }); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Best effort: leftover empty dirs are harmless, but a directory holding an
	// unrelated file must not be swept away by a delete.
	_ = os.Remove(filepath.Dir(p))
	s.notifyChange()
	return nil
}

// writeFileAtomic writes through a temp file and renames it into place, so no
// reader ever sees a partial template. 0600 because an extension can name
// private paths and a command line.
func writeFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-ext-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return retryFS(func() error { return os.Rename(tmpPath, p) })
}

// fsRetryAttempts and fsRetryDelay cover the window in which another goroutine
// holds a template open for reading. On Windows, syscall.Open does not pass
// FILE_SHARE_DELETE, so a rename or remove while a reader holds the handle fails
// with a sharing violation rather than waiting. The reconciler re-reads every
// template on a timer while an agent or the Web UI can save one, so this race is
// reachable in normal use. Both handles are transient, so a brief retry turns it
// back into the atomic swap it is everywhere else; on POSIX the first attempt
// already succeeded.
const (
	fsRetryAttempts = 20
	fsRetryDelay    = 25 * time.Millisecond
)

func retryFS(op func() error) error {
	var err error
	for i := 0; i < fsRetryAttempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		time.Sleep(fsRetryDelay)
	}
	return err
}
