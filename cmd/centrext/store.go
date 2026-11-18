package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"centrext/internal/infrastructure/keystore"
	"centrext/internal/infrastructure/sqlite"
)

// cmdStore shows the store's encryption or converts it to AES-XTS (ADR-025).
func cmdStore(ctx context.Context, args []string) error {
	if len(args) != 1 || (args[0] != "status" && args[0] != "reencrypt") {
		return errors.New("usage: centrext store status | reencrypt")
	}
	h, err := home()
	if err != nil {
		return err
	}
	path := filepath.Join(h, "centrext.db")
	key, _, err := keystore.Load(h, sqlite.KeySize, false)
	if err != nil {
		return err
	}
	if args[0] == "reencrypt" {
		backup, err := sqlite.Reencrypt(ctx, path, key)
		if err != nil {
			return err
		}
		if backup == "" {
			fmt.Println("The store already uses xts; nothing to do.")
			return nil
		}
		fmt.Printf("Re-encrypted %s with xts and verified the copy.\n", path)
		fmt.Printf("The old file is at %s (still encrypted). Delete it once your clients work.\n", backup)
		return nil
	}
	s, err := sqlite.Open(ctx, path, key)
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Printf("Store:  %s\nCipher: %s\nSchema: v%d\n", path, s.Cipher(), sqlite.SchemaVersion)
	if s.Cipher() != sqlite.XTS {
		fmt.Println("Upgrade with `centrext store reencrypt` after quitting your AI clients.")
	}
	return nil
}
