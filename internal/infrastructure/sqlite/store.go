// Package sqlite implements domain.Store on an encrypted local SQLite file
// (ADR-003, ADR-016, ADR-021).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"

	"centrext/internal/domain"
)

// ErrNewerSchema means the database was created by a newer Centrext.
var ErrNewerSchema = errors.New("database schema is newer than this centrext binary; upgrade centrext")

// KeySize is the encryption key length in bytes.
const KeySize = 32

// Store is a domain.Store backed by SQLite.
type Store struct {
	db *sql.DB
}

var _ domain.Store = (*Store)(nil)

// Open opens (creating if needed) the encrypted database at path and brings
// its schema up to date.
func Open(ctx context.Context, path string, key []byte) (*Store, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be %d bytes", KeySize)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Order matters: the key pragma must come first.
	dsn := "file:" + path + "?vfs=adiantum&_txlock=immediate" +
		"&_pragma=hexkey('" + hex.EncodeToString(key) + "')" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(full)" + // ADR-021: no acknowledged-write loss
		"&_pragma=foreign_keys(on)" +
		"&_pragma=temp_store(memory)"
	// FTS5 is a separate extension in this driver; register it on every
	// connection, since revision triggers write to the FTS table.
	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error { return fts5.Register(c) })
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "pragma user_version").Scan(&v); err != nil {
		return fmt.Errorf("open database (wrong key?): %w", err)
	}
	if v > SchemaVersion {
		return ErrNewerSchema
	}
	if v == SchemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil) // BEGIN IMMEDIATE: one migrator at a time
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Re-read inside the write lock: another process may have migrated already.
	if err := tx.QueryRowContext(ctx, "pragma user_version").Scan(&v); err != nil {
		return err
	}
	if v > SchemaVersion {
		return ErrNewerSchema
	}
	for ; v < SchemaVersion; v++ {
		if _, err := tx.ExecContext(ctx, migrations[v]); err != nil {
			return fmt.Errorf("migration to v%d: %w", v+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("pragma user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// InTx runs fn in a write transaction (BEGIN IMMEDIATE).
func (s *Store) InTx(ctx context.Context, fn func(domain.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&txn{tx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

// Read runs fn in a read-only transaction.
func (s *Store) Read(ctx context.Context, fn func(domain.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(&txn{tx: tx})
}
