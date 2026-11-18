package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/sqlite"
)

func TestNewStoresUseXTS(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "c.db")
	key := bytes.Repeat([]byte{3}, sqlite.KeySize)
	s, err := sqlite.Open(ctx, p, key)
	if err != nil {
		t.Fatal(err)
	}
	if s.Cipher() != sqlite.XTS {
		t.Fatalf("cipher = %s", s.Cipher())
	}
	s.Close()
	if s, err = sqlite.Open(ctx, p, key); err != nil || s.Cipher() != sqlite.XTS {
		t.Fatalf("reopen: %v", err)
	}
	s.Close()
	if _, err := sqlite.Open(ctx, p, bytes.Repeat([]byte{4}, sqlite.KeySize)); !errors.Is(err, sqlite.ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	if backup, err := sqlite.Reencrypt(ctx, p, key); err != nil || backup != "" {
		t.Fatalf("an xts store needs no re-encryption: %q %v", backup, err)
	}
}

func TestReencryptLegacyStore(t *testing.T) {
	ctx := context.Background()
	p := filepath.Join(t.TempDir(), "c.db")
	key := bytes.Repeat([]byte{5}, sqlite.KeySize)
	legacy, err := sqlite.OpenLegacy(ctx, p, key)
	if err != nil {
		t.Fatal(err)
	}
	svc := application.New(legacy)
	svc.Init(ctx, "owner")
	user, _ := svc.LocalUser(ctx)
	ns, _ := svc.FindNamespace(ctx, user, application.PersonalNamespace)
	res, err := svc.Propose(ctx, user, application.ProposeInput{NamespaceID: ns.ID, Kind: domain.KindCreate, Origin: domain.OriginUser,
		Type: domain.TypeDecision, Title: "Orders database", Body: "Orders live in PostgreSQL behind an outbox."})
	if err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	// A legacy store still opens, so nothing breaks before re-encryption.
	running, err := sqlite.Open(ctx, p, key)
	if err != nil || running.Cipher() != sqlite.Adiantum {
		t.Fatalf("legacy open: %v", err)
	}
	if _, err := sqlite.Reencrypt(ctx, p, key); !errors.Is(err, sqlite.ErrInUse) {
		t.Fatalf("re-encrypting while another connection is open must be refused: %v", err)
	}
	running.Close()

	backup, err := sqlite.Reencrypt(ctx, p, key)
	if err != nil || backup != p+".adiantum-backup" {
		t.Fatalf("reencrypt: %q %v", backup, err)
	}
	s, err := sqlite.Open(ctx, p, key)
	if err != nil || s.Cipher() != sqlite.XTS {
		t.Fatalf("after: %v", err)
	}
	defer s.Close()
	svc = application.New(s)
	found, err := svc.Search(ctx, user, application.SearchQuery{Query: "postgresql outbox"})
	if err != nil || len(found.Items) != 1 || found.Items[0].Context.ID != res.Context.Context.ID {
		t.Fatalf("data and FTS index must survive: %+v %v", found.Items, err)
	}
	if err := s.ExecRaw(`update revisions set body = 'changed'`); err == nil {
		t.Fatal("the triggers that keep revisions immutable must survive the copy")
	}
	if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("PostgreSQL")) || bytes.HasPrefix(b, []byte("SQLite format")) {
		t.Fatal("the new file must be encrypted")
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatal("the old file is kept as a backup")
	}
	if again, err := sqlite.Reencrypt(ctx, p, key); err != nil || again != "" {
		t.Fatalf("an xts store needs nothing: %q %v", again, err)
	}
}
