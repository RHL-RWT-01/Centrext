package sqlite

import "context"

// OpenLegacy creates or opens a store with the pre-ADR-025 Adiantum cipher.
func OpenLegacy(ctx context.Context, path string, key []byte) (*Store, error) {
	return openWith(ctx, path, key, Adiantum)
}

// ExecRaw runs SQL directly, for tests that check database-level guards.
func (s *Store) ExecRaw(q string) error {
	_, err := s.db.Exec(q)
	return err
}
