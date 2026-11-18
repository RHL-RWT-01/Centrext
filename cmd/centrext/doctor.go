package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"centrext/internal/application"
	"centrext/internal/connect"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/embeddings"
	"centrext/internal/infrastructure/keystore"
	"centrext/internal/infrastructure/sqlite"
)

// errDoctorFailed makes doctor exit 1 without printing another error line.
var errDoctorFailed = errors.New("doctor found problems")

// cmdDoctor checks the local install and connected clients. It changes
// nothing (opening the store may run pending migrations).
func cmdDoctor(ctx context.Context) error {
	var checks []connect.Check
	add := func(st connect.Status, area, msg, fix string) {
		checks = append(checks, connect.Check{Status: st, Area: area, Message: msg, Fix: fix})
	}
	defer func() { printChecks(checks) }()

	h, err := home()
	if err != nil {
		add(connect.Fail, "store", err.Error(), "")
		return errDoctorFailed
	}
	dbPath := filepath.Join(h, "centrext.db")
	fi, err := os.Stat(dbPath)
	if err != nil {
		add(connect.Fail, "store", "no database at "+dbPath, "centrext init")
		return errDoctorFailed
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 { // Windows uses ACLs, not modes
		add(connect.Warn, "store", fmt.Sprintf("%s is readable by other users (%v)", dbPath, fi.Mode().Perm()), "chmod 600 "+dbPath)
	}
	key, src, err := keystore.Load(h, sqlite.KeySize, false)
	if err != nil {
		add(connect.Fail, "store", "can't load the database key: "+err.Error(), "")
		return errDoctorFailed
	}
	if src == keystore.FromEnv {
		add(connect.Warn, "store", keystore.EnvKey+" is set and overrides the keychain; clients started without it can't open the database", "unset "+keystore.EnvKey)
	}
	if dir, files, problem := keystore.FileSecrets(h); problem != "" {
		add(connect.Fail, "keys", problem, "chmod -R go-rwx "+dir)
	} else if len(files) > 0 {
		add(connect.Warn, "keys", fmt.Sprintf("%d secret(s) are kept in %s instead of an OS keychain; a copy of %s includes them", len(files), dir, h),
			"leave "+dir+" out of backups, or use a keychain (ADR-026)")
	}
	store, err := sqlite.Open(ctx, dbPath, key)
	if err != nil {
		add(connect.Fail, "store", "can't open "+dbPath+": "+err.Error(), "")
		return errDoctorFailed
	}
	defer store.Close()
	svc := application.New(store)
	user, err := svc.LocalUser(ctx)
	if err != nil {
		add(connect.Fail, "store", "no local user: "+err.Error(), "centrext init")
		return errDoctorFailed
	}
	add(connect.OK, "store", fmt.Sprintf("%s opens (schema v%d, %s, key from %s)", dbPath, sqlite.SchemaVersion, store.Cipher(), src), "")
	if store.Cipher() != sqlite.XTS {
		add(connect.Warn, "store", "encrypted with "+string(store.Cipher())+"; new stores use xts (ADR-025)", "quit your AI clients, then run centrext store reencrypt")
	}
	if os.Getenv(keystore.EnvToken) != "" {
		add(connect.Warn, "environment", keystore.EnvToken+" is set; in this shell it overrides every client's keychain token", "unset "+keystore.EnvToken)
	}

	env, err := connectEnv()
	if err != nil { // e.g. a `go run` binary: still check, against the raw path
		add(connect.Warn, "binary", err.Error(), "")
		if env, err = connectEnvAt(""); err != nil {
			add(connect.Fail, "binary", err.Error(), "")
			return errDoctorFailed
		}
	}
	cwd, _ := os.Getwd()
	checks = append(checks, connect.Doctor(ctx, svc, user, env, cwd)...)
	checkEmbeddings(ctx, h, store, user, add)
	for _, c := range checks {
		if c.Status == connect.Fail {
			return errDoctorFailed
		}
	}
	return nil
}

func printChecks(checks []connect.Check) {
	fails, warns := 0, 0
	for _, c := range checks {
		fmt.Printf("%-5s %s: %s\n", c.Status, c.Area, c.Message)
		if c.Fix != "" && c.Status != connect.OK {
			fmt.Printf("      fix: %s\n", c.Fix)
		}
		switch c.Status {
		case connect.Fail:
			fails++
		case connect.Warn:
			warns++
		}
	}
	fmt.Printf("\n%d problem(s), %d warning(s)\n", fails, warns)
}

// checkEmbeddings reports semantic search setup without calling the API.
func checkEmbeddings(ctx context.Context, h string, store *sqlite.Store, user domain.Actor, add func(connect.Status, string, string, string)) {
	cfg, ok, err := embeddings.LoadConfig(h)
	switch {
	case err != nil:
		add(connect.Fail, "embeddings", err.Error(), "centrext embeddings enable")
		return
	case !ok:
		add(connect.OK, "embeddings", "off; search is lexical", "")
		return
	}
	key, src, err := keystore.LoadEmbeddingsKey(h, embeddings.KeyEnv(cfg.Provider))
	if err != nil {
		add(connect.Fail, "embeddings", "enabled ("+cfg.Model+") but "+err.Error(), "centrext embeddings key")
		return
	}
	if src != string(keystore.FromKeychain) {
		add(connect.Warn, "embeddings", "API key comes from "+src+"; clients started without it search lexically", "centrext embeddings key")
	}
	st, err := application.New(store, application.WithEmbedder(embeddings.New(cfg, key, embedderTimeout))).EmbeddingStatus(ctx, user, false)
	if err != nil {
		add(connect.Fail, "embeddings", err.Error(), "")
		return
	}
	if len(st.Eligible) == 0 {
		add(connect.Warn, "embeddings", "enabled, but every namespace is local_only, so nothing is embedded", "centrext namespace policy NS local_primary")
		return
	}
	add(connect.OK, "embeddings", fmt.Sprintf("%s: %d of %d items embedded in %d namespace(s)", st.Model, st.Embedded, st.Active, len(st.Eligible)), "")
}
