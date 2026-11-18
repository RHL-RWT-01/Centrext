package keystore_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"centrext/internal/infrastructure/keystore"
)

func TestKeychainIsPreferred(t *testing.T) {
	keyring.MockInit()
	home := t.TempDir()
	k, src, err := keystore.Load(home, 32, true)
	if err != nil || src != keystore.FromKeychain || len(k) != 32 {
		t.Fatalf("%v %v", src, err)
	}
	again, src, _ := keystore.Load(home, 32, false)
	if string(again) != string(k) || src != keystore.FromKeychain {
		t.Fatal("reload")
	}
	if _, err := os.Stat(filepath.Join(home, keystore.KeysDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("no files when the keychain works")
	}
}

func TestFileFallbackWithoutKeychain(t *testing.T) {
	keyring.MockInitWithError(errors.New("no D-Bus session"))
	home := t.TempDir()
	k, src, err := keystore.Load(home, 32, true)
	if err != nil || src != keystore.FromFile {
		t.Fatalf("init without a keychain should use the file store: %v %v", src, err)
	}
	if again, src, err := keystore.Load(home, 32, false); err != nil || string(again) != string(k) || src != keystore.FromFile {
		t.Fatalf("reload: %v %v", src, err)
	}
	dir, files, problem := keystore.FileSecrets(home)
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 || len(files) != 1 || problem != "" {
		t.Fatalf("keys folder %v, files %v, problem %q", fi.Mode().Perm(), files, problem)
	}
	if fi, _ := os.Stat(filepath.Join(dir, files[0])); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}

	if src, err := keystore.StoreAgentToken(home, "agt_1", "cxt_secret"); err != nil || src != keystore.FromFile {
		t.Fatal(err)
	}
	if tok, err := keystore.StoredAgentToken(home, "agt_1"); err != nil || tok != "cxt_secret" {
		t.Fatalf("token: %q %v", tok, err)
	}
	if err := keystore.DeleteAgentToken(home, "agt_1"); err != nil {
		t.Fatal(err)
	}
	// Without the file, the keychain is asked, and it's unreachable here.
	if tok, err := keystore.StoredAgentToken(home, "agt_1"); err == nil || tok != "" {
		t.Fatalf("deleted token still readable: %q", tok)
	}
	src2, err := keystore.StoreEmbeddingsKey(home, "sk-test")
	if err != nil || src2 != keystore.FromFile {
		t.Fatal(err)
	}
	if v, src, err := keystore.LoadEmbeddingsKey(home, []string{"CENTREXT_TEST_UNSET"}); v != "sk-test" || src != "file" || err != nil {
		t.Fatalf("embeddings key: %q %q %v", v, src, err)
	}

	os.Chmod(dir, 0o755)
	if _, _, problem := keystore.FileSecrets(home); problem == "" {
		t.Fatal("a keys folder other users can open must be reported")
	}
}

// A keychain that's unavailable must never lead to a second key for a
// database that already exists: its key may be in that keychain.
func TestNoNewKeyForExistingDatabase(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "centrext.db"), []byte("x"), 0o600)
	if _, _, err := keystore.Load(home, 32, true); err == nil {
		t.Fatal("must not create a key while the keychain is unreachable and a database exists")
	}
	keyring.MockInit()
	if _, _, err := keystore.Load(home, 32, true); err == nil {
		t.Fatal("must not create a key for a database whose key is missing")
	}
}

func TestForcedFileStore(t *testing.T) {
	keyring.MockInit()
	t.Setenv(keystore.EnvStore, "file")
	home := t.TempDir()
	if _, src, err := keystore.Load(home, 32, true); err != nil || src != keystore.FromFile {
		t.Fatalf("%v %v", src, err)
	}
}
