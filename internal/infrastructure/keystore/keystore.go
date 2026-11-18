// Package keystore keeps the local database key in the OS keychain (section 5).
package keystore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/zalando/go-keyring"
)

const (
	service = "centrext"
	// EnvKey overrides the keychain with a 64-hex-digit key. Meant for tests
	// and machines without a keychain until the Linux fallback (ADR-021) exists.
	EnvKey = "CENTREXT_DB_KEY"
)

// Source says where the key came from.
type Source string

const (
	FromEnv      Source = "environment"
	FromKeychain Source = "keychain"
)

// ErrNoKey means no key exists yet.
var ErrNoKey = errors.New("no database key found; run `centrext init`")

func account(home string) string { return "local-db-key:" + home }

// Load returns the key for the Centrext home directory. With create set, a
// missing key is generated and stored in the keychain.
func Load(home string, size int, create bool) ([]byte, Source, error) {
	if v := os.Getenv(EnvKey); v != "" {
		k, err := hex.DecodeString(v)
		if err != nil || len(k) != size {
			return nil, "", fmt.Errorf("%s must be %d hex digits", EnvKey, size*2)
		}
		return k, FromEnv, nil
	}
	v, err := keyring.Get(service, account(home))
	if err == nil {
		k, err := hex.DecodeString(v)
		if err != nil || len(k) != size {
			return nil, "", fmt.Errorf("keychain entry for %s is malformed", home)
		}
		return k, FromKeychain, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return nil, "", fmt.Errorf("keychain unavailable (%v); set %s as a temporary workaround", err, EnvKey)
	}
	if !create {
		return nil, "", ErrNoKey
	}
	k := make([]byte, size)
	if _, err := rand.Read(k); err != nil {
		return nil, "", err
	}
	if err := keyring.Set(service, account(home), hex.EncodeToString(k)); err != nil {
		return nil, "", fmt.Errorf("store key in keychain: %w", err)
	}
	return k, FromKeychain, nil
}

// EnvToken supplies an agent token where there is no keychain (ADR-018).
const EnvToken = "CENTREXT_TOKEN"

// ErrNoToken means no token is stored for the agent.
var ErrNoToken = errors.New("no token for this agent; run `centrext connect` or set " + EnvToken)

func tokenAccount(home, agentID string) string { return "agent-token:" + home + ":" + agentID }

// StoreAgentToken keeps an agent's token in the keychain, so client config
// files only need the non-secret agent ID.
func StoreAgentToken(home, agentID, token string) error {
	return keyring.Set(service, tokenAccount(home, agentID), token)
}

// LoadAgentToken returns the token from CENTREXT_TOKEN, or from the keychain
// by agent ID.
func LoadAgentToken(home, agentID string) (string, error) {
	if v := os.Getenv(EnvToken); v != "" {
		return v, nil
	}
	return StoredAgentToken(home, agentID)
}

// StoredAgentToken returns the token kept in the keychain, ignoring CENTREXT_TOKEN.
func StoredAgentToken(home, agentID string) (string, error) {
	if agentID == "" {
		return "", ErrNoToken
	}
	v, err := keyring.Get(service, tokenAccount(home, agentID))
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoToken
	}
	if err != nil {
		return "", fmt.Errorf("keychain unavailable (%v); set %s", err, EnvToken)
	}
	return v, nil
}

// DeleteAgentToken removes a stored token (e.g. after revocation).
func DeleteAgentToken(home, agentID string) error {
	err := keyring.Delete(service, tokenAccount(home, agentID))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
