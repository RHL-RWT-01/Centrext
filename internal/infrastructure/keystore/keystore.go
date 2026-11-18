// Package keystore keeps Centrext's secrets: the local database key, agent
// tokens and the embeddings API key. They live in the OS keychain, or in
// protected files under CENTREXT_HOME/keys where there is no keychain
// (section 5, ADR-018, ADR-026).
package keystore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"

	"centrext/internal/fsutil"
)

const (
	service = "centrext"
	// EnvKey overrides the stored database key with 64 hex digits (tests, CI).
	EnvKey = "CENTREXT_DB_KEY"
	// EnvToken supplies an agent token, overriding the stored one.
	EnvToken = "CENTREXT_TOKEN"
	// EnvStore set to "file" forces the file store (ADR-026).
	EnvStore = "CENTREXT_KEY_STORE"
	// KeysDir holds the file store, under CENTREXT_HOME.
	KeysDir = "keys"
)

// Source says where a secret came from.
type Source string

const (
	FromEnv      Source = "environment"
	FromKeychain Source = "keychain"
	FromFile     Source = "file"
)

// ErrNoKey means no database key exists yet.
var ErrNoKey = errors.New("no database key found; run `centrext init`")

// ErrNoToken means no token is stored for the agent.
var ErrNoToken = errors.New("no token for this agent; run `centrext connect` or set " + EnvToken)

// ErrNoEmbeddingsKey means no API key is stored or set in the environment.
var ErrNoEmbeddingsKey = errors.New("no embeddings API key; run `centrext embeddings key`")

var errMissing = errors.New("not stored")

// secret names one stored secret: its keychain account and its file name.
type secret struct{ home, account, file string }

func dbKey(home string) secret {
	return secret{home, "local-db-key:" + home, "db.key"}
}

func agentToken(home, id string) secret {
	return secret{home, "agent-token:" + home + ":" + id, "agent-" + id + ".token"}
}

func embeddingsKey(home string) secret {
	return secret{home, "embeddings-key:" + home, "embeddings.key"}
}

func (s secret) path() string { return filepath.Join(s.home, KeysDir, s.file) }

func forceFile() bool { return os.Getenv(EnvStore) == "file" }

// get reads a secret: the file first (it only exists if it was written
// there), then the keychain. errMissing means it's in neither.
func (s secret) get() (string, Source, error) {
	if b, err := os.ReadFile(s.path()); err == nil {
		return strings.TrimSpace(string(b)), FromFile, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	if forceFile() {
		return "", "", errMissing
	}
	v, err := keyring.Get(service, s.account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", "", errMissing
	}
	if err != nil {
		return "", "", fmt.Errorf("keychain unavailable (%v); on machines without one, set %s=file", err, EnvStore)
	}
	return v, FromKeychain, nil
}

// set stores a secret in the keychain, or in a file if the keychain is
// unavailable or the file store is forced.
func (s secret) set(v string) (Source, error) {
	if !forceFile() {
		if err := keyring.Set(service, s.account, v); err == nil {
			return FromKeychain, nil
		}
	}
	dir := filepath.Join(s.home, KeysDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	if err := fsutil.WriteAtomic(s.path(), []byte(v+"\n"), 0o600); err != nil {
		return "", err
	}
	return FromFile, os.Chmod(s.path(), 0o600)
}

// del removes a secret from both stores. When it was in a file, an
// unreachable keychain doesn't matter: a secret is only ever in one place.
func (s secret) del() error {
	err := os.Remove(s.path())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil || forceFile() {
		return nil
	}
	if err := keyring.Delete(service, s.account); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// Load returns the database key for the Centrext home directory. With create
// set, a missing key is generated and stored. A keychain that's unavailable
// never leads to a new key while a database may exist (ADR-026).
func Load(home string, size int, create bool) ([]byte, Source, error) {
	if v := os.Getenv(EnvKey); v != "" {
		k, err := hex.DecodeString(v)
		if err != nil || len(k) != size {
			return nil, "", fmt.Errorf("%s must be %d hex digits", EnvKey, size*2)
		}
		return k, FromEnv, nil
	}
	s := dbKey(home)
	v, src, err := s.get()
	if err == nil {
		k, err := hex.DecodeString(v)
		if err != nil || len(k) != size {
			return nil, "", fmt.Errorf("stored database key for %s is malformed", home)
		}
		return k, src, nil
	}
	if !create {
		if errors.Is(err, errMissing) {
			return nil, "", ErrNoKey
		}
		return nil, "", err
	}
	// Never make a new key for an existing database: its key may be in a
	// keychain that's locked or unreachable right now.
	if _, statErr := os.Stat(filepath.Join(home, "centrext.db")); statErr == nil {
		if errors.Is(err, errMissing) {
			return nil, "", fmt.Errorf("%s exists but its key is missing; a new key can't read it", filepath.Join(home, "centrext.db"))
		}
		return nil, "", err
	}
	k := make([]byte, size)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	src, err = s.set(hex.EncodeToString(k))
	if err != nil {
		return nil, "", fmt.Errorf("store database key: %w", err)
	}
	return k, src, nil
}

// StoreAgentToken keeps an agent's token, so client config files only need
// the non-secret agent ID. It says where the token went.
func StoreAgentToken(home, agentID, token string) (Source, error) {
	return agentToken(home, agentID).set(token)
}

// LoadAgentToken returns the token from CENTREXT_TOKEN, or the stored one.
func LoadAgentToken(home, agentID string) (string, error) {
	if v := os.Getenv(EnvToken); v != "" {
		return v, nil
	}
	return StoredAgentToken(home, agentID)
}

// StoredAgentToken returns the stored token, ignoring CENTREXT_TOKEN.
func StoredAgentToken(home, agentID string) (string, error) {
	if agentID == "" {
		return "", ErrNoToken
	}
	v, _, err := agentToken(home, agentID).get()
	if errors.Is(err, errMissing) {
		return "", ErrNoToken
	}
	return v, err
}

// DeleteAgentToken removes a stored token (e.g. after revocation).
func DeleteAgentToken(home, agentID string) error { return agentToken(home, agentID).del() }

// StoreEmbeddingsKey keeps the hosted embeddings API key (ADR-023) and says where.
func StoreEmbeddingsKey(home, key string) (Source, error) { return embeddingsKey(home).set(key) }

// LoadEmbeddingsKey returns the key from the first set variable in envs, else
// the stored one. The second result names where it came from.
func LoadEmbeddingsKey(home string, envs []string) (string, string, error) {
	for _, e := range envs {
		if v := os.Getenv(e); v != "" {
			return v, e, nil
		}
	}
	v, src, err := embeddingsKey(home).get()
	if errors.Is(err, errMissing) {
		return "", "", ErrNoEmbeddingsKey
	}
	return v, string(src), err
}

// DeleteEmbeddingsKey removes the stored key.
func DeleteEmbeddingsKey(home string) error { return embeddingsKey(home).del() }

// FileSecrets lists the secrets kept in files, for doctor. It returns the
// keys folder and any problem with its permissions.
func FileSecrets(home string) (dir string, files []string, problem string) {
	dir = filepath.Join(home, KeysDir)
	fi, err := os.Stat(dir)
	if err != nil {
		return dir, nil, ""
	}
	if fi.Mode().Perm()&0o077 != 0 {
		problem = fmt.Sprintf("%s is accessible by other users (%v)", dir, fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files = append(files, e.Name())
		if info, err := e.Info(); err == nil && info.Mode().Perm()&0o077 != 0 && problem == "" {
			problem = fmt.Sprintf("%s is readable by other users (%v)", filepath.Join(dir, e.Name()), info.Mode().Perm())
		}
	}
	return dir, files, problem
}
