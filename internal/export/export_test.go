package export_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/export"
	"centrext/internal/infrastructure/sqlite"
	"centrext/internal/workspace"
)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	svc  *application.Service
	user domain.Actor
	repo string
	ns   domain.Namespace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "c.db"), bytes.Repeat([]byte{5}, sqlite.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := application.New(s)
	svc.Init(ctx, "owner")
	user, _ := svc.LocalUser(ctx)
	repo := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[remote \"origin\"]\n\turl = git@github.com:acme/shop.git\n"), 0o644)
	id, _ := workspace.Resolve(repo)
	r, err := svc.ResolveNamespace(ctx, user, id)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, ctx: ctx, svc: svc, user: user, repo: repo, ns: r.Namespace}
}

func (f *fixture) add(actor domain.Actor, origin domain.ProposalOrigin, typ domain.ContextType, title, body string) domain.Context {
	f.t.Helper()
	res, err := f.svc.Propose(f.ctx, actor, application.ProposeInput{NamespaceID: f.ns.ID, Kind: domain.KindCreate,
		Origin: origin, Type: typ, Title: title, Body: body})
	if err != nil {
		f.t.Fatal(err)
	}
	return res.Context.Context
}

func (f *fixture) export() export.Result {
	f.t.Helper()
	ns, views, err := f.svc.ExportSnapshot(f.ctx, f.user, f.ns.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	res, err := export.Write(f.repo, ns, views)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *fixture) read(rel string) string {
	b, _ := os.ReadFile(filepath.Join(f.repo, rel))
	return string(b)
}

func TestExportNeedsPolicyThatAllowsLeavingTheDevice(t *testing.T) {
	f := newFixture(t)
	if f.ns.StorageMode != domain.StorageLocalOnly {
		t.Fatal("auto-created repo namespaces should be local_only")
	}
	_, _, err := f.svc.ExportSnapshot(f.ctx, f.user, f.ns.ID)
	if !errors.Is(err, domain.ErrForbidden) || !strings.Contains(err.Error(), "centrext namespace policy") {
		t.Fatalf("local_only export: %v", err)
	}
	_, tok, _ := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "a", Client: "cursor", AllNamespaces: true})
	agent, _ := f.svc.Authenticate(f.ctx, tok)
	if _, err := f.svc.SetStorageMode(f.ctx, agent, f.ns.ID, domain.StorageLocalPrimary); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("agents must not change storage policy")
	}
	if _, _, err := f.svc.ExportSnapshot(f.ctx, agent, f.ns.ID); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("agents must not export")
	}
	if _, err := f.svc.SetStorageMode(f.ctx, f.user, f.ns.ID, "anywhere"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("unknown modes must be refused")
	}
}

func TestExportWritesOnlyVerifiedDecisionsAndStaysStable(t *testing.T) {
	f := newFixture(t)
	if _, err := f.svc.SetStorageMode(f.ctx, f.user, f.ns.ID, domain.StorageLocalPrimary); err != nil {
		t.Fatal(err)
	}
	_, tok, _ := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "a", Client: "claude-code", AllNamespaces: true})
	agent, _ := f.svc.Authenticate(f.ctx, tok)

	db := f.add(f.user, domain.OriginUser, domain.TypeDecision, "Orders in PostgreSQL", "Use PostgreSQL with an outbox.")
	f.add(f.user, domain.OriginUser, domain.TypeConvention, "Money as cents", "Store int64 cents.")
	pending := f.add(agent, domain.OriginExplicit, domain.TypeDecision, "Stripe for payments", "Hosted checkout.")
	f.add(f.user, domain.OriginUser, domain.TypeFact, "On-call", "Mondays.")
	f.add(agent, domain.OriginSilent, domain.TypeObservation, "Flaky test", "TestCheckout is flaky.")

	res := f.export()
	if len(res.Written) != 3 { // 2 items + README
		t.Fatalf("written: %v", res.Written)
	}
	index := f.read(".centrext-export/README.md")
	for _, want := range []string{"## Decisions", "Orders in PostgreSQL", "## Conventions", "Money as cents", "Don't edit"} {
		if !strings.Contains(index, want) {
			t.Fatalf("index missing %q:\n%s", want, index)
		}
	}
	for _, unwanted := range []string{"Stripe", "On-call", "Flaky"} {
		if strings.Contains(index, unwanted) {
			t.Fatalf("index must only list user_verified decisions, architecture and conventions; found %q", unwanted)
		}
	}
	item := f.read(filepath.Join(export.Dir, "context", "orders-in-postgresql-"+db.ID[len(db.ID)-6:]+".md"))
	if !strings.Contains(item, "trust: user_verified") || !strings.Contains(item, "# Orders in PostgreSQL") || !strings.Contains(item, "outbox") {
		t.Fatalf("item file:\n%s", item)
	}

	if again := f.export(); len(again.Written) != 0 || len(again.Removed) != 0 {
		t.Fatalf("re-export without changes must not touch files: %+v", again)
	}

	// Verifying the agent's item adds it; deleting one removes its file but
	// leaves hand-written files alone.
	if _, err := f.svc.VerifyContext(f.ctx, f.user, pending.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteContext(f.ctx, f.user, db.ID, 1); err != nil {
		t.Fatal(err)
	}
	hand := filepath.Join(f.repo, export.Dir, "context", "notes.md")
	os.WriteFile(hand, []byte("my notes\n"), 0o644)
	res = f.export()
	if len(res.Removed) != 1 || !strings.Contains(res.Removed[0], "orders-in-postgresql") {
		t.Fatalf("removed: %v", res.Removed)
	}
	if index := f.read(".centrext-export/README.md"); !strings.Contains(index, "Stripe for payments") || strings.Contains(index, "Orders in PostgreSQL") {
		t.Fatalf("index after changes:\n%s", index)
	}
	if _, err := os.Stat(hand); err != nil {
		t.Fatal("files Centrext didn't generate must be kept")
	}
}

func TestFileNames(t *testing.T) {
	v := application.ContextView{Context: domain.Context{ID: "ctx_0123456789abcdef"}, Head: domain.Revision{Title: "  Use Postgres 16 (not MySQL)!  "}}
	if got := export.FileName(v); got != "use-postgres-16-not-mysql-abcdef.md" {
		t.Fatalf("got %q", got)
	}
	v.Head.Title = "日本語だけ"
	if got := export.FileName(v); got != "item-abcdef.md" {
		t.Fatalf("got %q", got)
	}
}
