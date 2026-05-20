package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/solcreek/creekd/internal/supervisor"
)

// FormatVersion is the schema version embedded in every written
// state file. Bump when the on-disk shape changes; readers refuse
// versions they don't recognise.
const FormatVersion = 1

// State is the on-disk shape of the persisted app set.
type State struct {
	Version int   `json:"version"`
	Apps    []App `json:"apps"`
}

// App is one persisted entry. The supervisor.Config is stored
// verbatim — on restore the same Spawn call reconstructs the same
// runtime state (modulo ephemeral fields like PID).
type App struct {
	Config supervisor.Config `json:"config"`
}

// Store serialises persisted state under a JSON file. Construct
// with NewStore; every mutation flushes synchronously to disk.
type Store struct {
	path string

	mu   sync.Mutex
	apps map[string]supervisor.Config // appID → config
}

// NewStore opens (or creates) the state file at path. If the file
// exists it is loaded into the in-memory cache; missing files are
// treated as an empty state. The parent directory is created on
// first save. Returns an error only on permission / unreadable-file
// failure — an empty file or absent file is normal.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path: path,
		apps: make(map[string]supervisor.Config),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the configured file path.
func (s *Store) Path() string { return s.path }

// Apps returns a snapshot of every persisted config in alphabetical
// order. Useful for replaying through supervisor.Spawn at startup.
func (s *Store) Apps() []supervisor.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.apps))
	for id := range s.apps {
		ids = append(ids, id)
	}
	sortStrings(ids)
	out := make([]supervisor.Config, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.apps[id])
	}
	return out
}

// AddApp persists cfg. If an entry with the same ID already exists
// it is replaced — admin Spawn rejects duplicates upstream, so this
// behaviour matters only for the Deploy path which legitimately
// overwrites.
//
// Copy-on-write: builds a candidate map, flushes that to disk, and
// only on flush success does it swap s.apps. If the flush fails the
// in-memory cache stays at the pre-call state, matching the on-disk
// state. Without this, a failed flush would leave in-memory ahead of
// disk; a subsequent successful flush would then persist what was
// reported as failed.
func (s *Store) AddApp(cfg supervisor.Config) error {
	if cfg.ID == "" {
		return errors.New("state: empty app id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneMap(s.apps)
	next[cfg.ID] = cfg
	if err := s.flushMap(next); err != nil {
		return err
	}
	s.apps = next
	return nil
}

// RemoveApp drops the entry for id. Removing an unknown id is a
// no-op (the on-disk state is already in sync). Same copy-on-write
// rule as AddApp — a flush failure leaves the in-memory cache intact.
func (s *Store) RemoveApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return nil
	}
	next := cloneMap(s.apps)
	delete(next, id)
	if err := s.flushMap(next); err != nil {
		return err
	}
	s.apps = next
	return nil
}

// load reads the JSON file into the in-memory cache. Missing file
// → empty cache, no error. Corrupted JSON or unknown version → error.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	if st.Version != FormatVersion {
		return fmt.Errorf("state: unsupported version %d (want %d)",
			st.Version, FormatVersion)
	}
	for _, a := range st.Apps {
		if a.Config.ID == "" {
			continue
		}
		s.apps[a.Config.ID] = a.Config
	}
	return nil
}

// flushMap writes the given map snapshot to disk atomically. Used by
// AddApp / RemoveApp after they build a candidate "next" map but
// before they swap it into s.apps. Caller must hold s.mu.
func (s *Store) flushMap(m map[string]supervisor.Config) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	st := State{Version: FormatVersion}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		st.Apps = append(st.Apps, App{Config: m[id]})
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("state: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename %s → %s: %w", tmp, s.path, err)
	}
	return nil
}

// cloneMap returns a shallow copy of src. supervisor.Config is a
// value type, so a shallow copy is a full copy.
func cloneMap(src map[string]supervisor.Config) map[string]supervisor.Config {
	dst := make(map[string]supervisor.Config, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// sortStrings sorts in-place. Inlined to avoid an explicit "sort"
// import for the one-call use case.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
