package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/ncruces/go-sqlite3/driver"
)

// Cipher names an encrypting VFS (ADR-021, ADR-025).
type Cipher string

const (
	// XTS (AES-XTS) is used for new stores.
	XTS Cipher = "xts"
	// Adiantum was used before ADR-025; such stores still open, and
	// `centrext store reencrypt` converts them.
	Adiantum Cipher = "adiantum"
)

// ErrWrongKey means the file opened with neither cipher: the key is wrong,
// or it isn't a Centrext database.
var ErrWrongKey = errors.New("can't read the database: wrong key, or not a Centrext database")

// detectCipher returns the cipher of the file at path, or XTS for a new one.
// Probes are read-only, so a wrong guess never writes.
func detectCipher(ctx context.Context, path string, key []byte) (Cipher, error) {
	if fi, err := os.Stat(path); errors.Is(err, os.ErrNotExist) || (err == nil && fi.Size() == 0) {
		return XTS, nil
	}
	for _, c := range []Cipher{XTS, Adiantum} {
		if probe(ctx, path, key, c) {
			return c, nil
		}
	}
	return "", ErrWrongKey
}

func probe(ctx context.Context, path string, key []byte, c Cipher) bool {
	db, err := driver.Open("file:"+path+"?vfs="+string(c)+"&mode=ro&_pragma=hexkey('"+hex.EncodeToString(key)+"')&_pragma=busy_timeout(5000)", nil)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	return db.QueryRowContext(ctx, "select count(*) from sqlite_schema").Scan(&n) == nil
}

// ErrInUse means another connection has the database open.
var ErrInUse = errors.New("the database is in use; quit your AI clients (Claude Code, Cursor, Claude Desktop) and try again")

// Reencrypt converts an Adiantum store at path to XTS with the same key
// (ADR-025). It needs exclusive access, verifies the copy, and keeps the old
// file as path + ".adiantum-backup". It returns the backup path, or "" if
// the store already uses XTS.
func Reencrypt(ctx context.Context, path string, key []byte) (string, error) {
	c, err := detectCipher(ctx, path, key)
	if err != nil || c == XTS {
		return "", err
	}
	backup := path + ".adiantum-backup"
	if _, err := os.Stat(backup); err == nil {
		return "", fmt.Errorf("%s already exists; move it aside first", backup)
	}
	old, err := driver.Open("file:"+path+"?vfs="+string(Adiantum)+"&_pragma=hexkey('"+hex.EncodeToString(key)+"')&_pragma=busy_timeout(0)", registerFTS)
	if err != nil {
		return "", err
	}
	defer old.Close()
	old.SetMaxOpenConns(1)
	conn, err := old.Conn(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	// Leaving WAL mode needs every other connection gone, so this fails
	// while a client is running. Then hold the lock for the whole copy.
	var mode string
	if err := conn.QueryRowContext(ctx, "pragma journal_mode=delete").Scan(&mode); err != nil || mode != "delete" {
		return "", ErrInUse
	}
	if _, err := conn.ExecContext(ctx, "pragma locking_mode=exclusive"); err != nil {
		return "", err
	}
	if _, err := conn.ExecContext(ctx, "begin exclusive; commit"); err != nil {
		return "", ErrInUse
	}
	tmp := path + ".reencrypt"
	for _, f := range []string{tmp, tmp + "-journal", tmp + "-wal", tmp + "-shm"} {
		os.Remove(f)
	}
	if _, err := conn.ExecContext(ctx, "vacuum into ?", "file:"+tmp+"?vfs="+string(XTS)+"&hexkey="+hex.EncodeToString(key)); err != nil {
		return "", fmt.Errorf("copy to xts: %w", err)
	}
	if err := verifyCopy(ctx, conn, tmp, key); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("the re-encrypted copy didn't verify, nothing changed: %w", err)
	}
	if err := os.Rename(path, backup); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Rename(backup, path)
		return "", err
	}
	os.Chmod(path, 0o600)
	os.Chmod(backup, 0o600)
	return backup, nil
}

// verifyCopy checks the copy's integrity, schema version and row counts
// against the original.
func verifyCopy(ctx context.Context, src *sql.Conn, path string, key []byte) error {
	dst, err := driver.Open("file:"+path+"?vfs="+string(XTS)+"&_pragma=hexkey('"+hex.EncodeToString(key)+"')", registerFTS)
	if err != nil {
		return err
	}
	defer dst.Close()
	var ok string
	if err := dst.QueryRowContext(ctx, "pragma integrity_check").Scan(&ok); err != nil || ok != "ok" {
		return fmt.Errorf("integrity_check: %s %v", ok, err)
	}
	var v1, v2 int
	src.QueryRowContext(ctx, "pragma user_version").Scan(&v1)
	dst.QueryRowContext(ctx, "pragma user_version").Scan(&v2)
	if v1 != v2 {
		return fmt.Errorf("schema version %d, want %d", v2, v1)
	}
	rows, err := src.QueryContext(ctx, "select name from sqlite_schema where type = 'table' and name not like 'sqlite_%'")
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	for _, t := range tables {
		var a, b int
		q := `select count(*) from "` + t + `"`
		if err := src.QueryRowContext(ctx, q).Scan(&a); err != nil {
			return err
		}
		if err := dst.QueryRowContext(ctx, q).Scan(&b); err != nil || a != b {
			return fmt.Errorf("table %s: %d rows, want %d (%v)", t, b, a, err)
		}
	}
	return nil
}
