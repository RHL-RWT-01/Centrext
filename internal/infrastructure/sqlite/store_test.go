package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"centrext/internal/domain"
)

var key = []byte("0123456789abcdef0123456789abcdef")

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := Open(context.Background(), path, key)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	now := time.Now()
	err := s.InTx(context.Background(), func(tx domain.Tx) error {
		if err := tx.InsertNamespace(domain.Namespace{ID: "ns1", Name: "P", StorageMode: domain.StorageLocalOnly, CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.InsertContext(domain.Context{ID: "c1", NamespaceID: "ns1", Type: domain.TypeFact, Status: domain.StatusActive,
			CurrentVersion: 1, HeadTrust: domain.TrustUserVerified, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := tx.InsertRevision(domain.Revision{ID: "r1", ContextID: "c1", Version: 1, Title: "PLAINTEXT-MARKER-T",
			Body: "PLAINTEXT-MARKER-B", Trust: domain.TrustUserVerified, AuthorKind: domain.ActorUser, AuthorID: "u", CreatedAt: now}); err != nil {
			return err
		}
		return tx.AppendAudit(domain.AuditEvent{ID: "a1", At: now, ActorKind: domain.ActorUser, ActorID: "u", Action: "x", Subject: "y", Detail: "{}"})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Invariant 3 and the append-only audit log are enforced by the database itself.
func TestRevisionsAndAuditAreImmutable(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	seed(t, s)
	for _, q := range []string{
		`update revisions set body='changed' where id='r1'`,
		`delete from revisions where id='r1'`,
		`update audit_events set action='changed' where id='a1'`,
		`delete from audit_events where id='a1'`,
	} {
		if _, err := s.db.Exec(q); err == nil {
			t.Fatalf("%q should be refused", q)
		}
	}
}

func TestDuplicateRevisionVersionIsStale(t *testing.T) {
	s, _ := openTemp(t)
	defer s.Close()
	seed(t, s)
	err := s.InTx(context.Background(), func(tx domain.Tx) error {
		return tx.InsertRevision(domain.Revision{ID: "r2", ContextID: "c1", Version: 1, Title: "t", Body: "b",
			Trust: domain.TrustUserVerified, AuthorKind: domain.ActorUser, AuthorID: "u", CreatedAt: time.Now()})
	})
	if !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("err = %v", err)
	}
}

func TestEncryptedAtRestAndWrongKey(t *testing.T) {
	s, path := openTemp(t)
	seed(t, s)
	s.Close()
	files, _ := filepath.Glob(path + "*")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if bytes.Contains(b, []byte("PLAINTEXT-MARKER")) || bytes.Contains(b, []byte("SQLite format 3")) {
			t.Fatalf("plaintext found in %s", filepath.Base(f))
		}
	}
	bad := bytes.Repeat([]byte{7}, KeySize)
	if s2, err := Open(context.Background(), path, bad); err == nil {
		s2.Close()
		t.Fatal("wrong key should fail to open")
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 { // Windows has no Unix modes
		t.Fatalf("db file mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, path := openTemp(t)
	if _, err := s.db.Exec(`pragma user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), path, key); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("err = %v", err)
	}
}

func TestReopenKeepsData(t *testing.T) {
	s, path := openTemp(t)
	seed(t, s)
	s.Close()
	s2, err := Open(context.Background(), path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	err = s2.Read(context.Background(), func(tx domain.Tx) error {
		r, err := tx.GetRevision("c1", 1)
		if err == nil && r.Body != "PLAINTEXT-MARKER-B" {
			t.Fatal("wrong body")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
