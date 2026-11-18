// Package session remembers which context ids hooks already delivered in a
// client session, so per-prompt injection doesn't repeat them (ADR-024).
// It stores ids only, never prompts or content.
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"centrext/internal/fsutil"
)

// Dir is the folder under CENTREXT_HOME.
const Dir = "sessions"

// MaxAge is how long a session's record is kept after its last change.
const MaxAge = 7 * 24 * time.Hour

// Delivered is one session's record.
type Delivered struct {
	path string
	IDs  []string `json:"ids"`
}

func file(home, sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	return filepath.Join(home, Dir, hex.EncodeToString(h[:12])+".json")
}

// Load returns the record for a session; a missing or unreadable one is empty.
// An empty session id gives a record that is never saved.
func Load(home, sessionID string) *Delivered {
	d := &Delivered{}
	if sessionID == "" {
		return d
	}
	d.path = file(home, sessionID)
	if b, err := os.ReadFile(d.path); err == nil {
		_ = json.Unmarshal(b, d)
	}
	return d
}

// Add records ids and reports whether any were new.
func (d *Delivered) Add(ids ...string) bool {
	added := false
	for _, id := range ids {
		if !slices.Contains(d.IDs, id) {
			d.IDs = append(d.IDs, id)
			added = true
		}
	}
	return added
}

// Save writes the record and removes records older than MaxAge.
func (d *Delivered) Save(now time.Time) error {
	if d.path == "" {
		return nil
	}
	b, _ := json.Marshal(d)
	if err := fsutil.WriteAtomic(d.path, b, 0o600); err != nil {
		return err
	}
	entries, _ := os.ReadDir(filepath.Dir(d.path))
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > MaxAge {
			os.Remove(filepath.Join(filepath.Dir(d.path), e.Name()))
		}
	}
	return nil
}
