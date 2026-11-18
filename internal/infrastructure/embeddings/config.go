package embeddings

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"centrext/internal/fsutil"
)

// ConfigFile holds the embeddings settings in the Centrext home. It never
// holds the API key, which lives in the keychain or the environment.
const ConfigFile = "embeddings.json"

// LoadConfig returns the saved config, or ok=false if embeddings aren't enabled.
func LoadConfig(home string) (cfg Config, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(home, ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, false, errors.New(ConfigFile + " is not valid JSON; run `centrext embeddings enable` again")
	}
	cfg, err = cfg.Defaults()
	return cfg, err == nil, err
}

// SaveConfig writes cfg (after Defaults) to the Centrext home.
func SaveConfig(home string, cfg Config) error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return fsutil.WriteAtomic(filepath.Join(home, ConfigFile), append(b, '\n'), 0o600)
}

// RemoveConfig disables embeddings. Stored vectors are kept.
func RemoveConfig(home string) error {
	err := os.Remove(filepath.Join(home, ConfigFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
