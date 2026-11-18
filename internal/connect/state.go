// Package connect installs Centrext into AI clients (`centrext connect`):
// it registers an agent per client, keeps its token in the keychain, writes
// the client's MCP config and session-start delivery (ADR-014, ADR-018), and
// keeps generated files such as the Cursor rules file current.
package connect

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State is what connect remembers locally, in CENTREXT_HOME/connections.json.
// It holds no secrets: tokens live in the keychain.
type State struct {
	// Agents maps a client to the agent connected for it, so client configs
	// can say `--client cursor` and survive token rotation.
	Agents map[string]string `json:"agents"`
	// CursorProjects maps a project folder to the namespace its rules file shows.
	CursorProjects map[string]string `json:"cursor_projects"`
}

const stateFile = "connections.json"

// LoadState reads the state; a missing file is an empty state.
func LoadState(centrextHome string) (State, error) {
	st := State{Agents: map[string]string{}, CursorProjects: map[string]string{}}
	b, err := os.ReadFile(filepath.Join(centrextHome, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Agents == nil {
		st.Agents = map[string]string{}
	}
	if st.CursorProjects == nil {
		st.CursorProjects = map[string]string{}
	}
	return st, nil
}

func (st State) save(centrextHome string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(centrextHome, stateFile), append(b, '\n'), 0o600)
}

// AgentFor returns the agent connected for a client.
func AgentFor(centrextHome, client string) (string, error) {
	st, err := LoadState(centrextHome)
	if err != nil {
		return "", err
	}
	id, ok := st.Agents[client]
	if !ok {
		return "", errors.New(client + " isn't connected; run `centrext connect " + client + "`")
	}
	return id, nil
}
