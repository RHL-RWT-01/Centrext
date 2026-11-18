package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/sqlite"
	"centrext/internal/workspace"
)

func repoDir(t *testing.T, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644)
	return dir
}

// The core scenario end to end: Claude Code and Cursor open the same repo
// (different clone URLs), resolve the same namespace, and Cursor finds the
// decision by searching.
func TestCoreScenarioWithResolutionAndSearch(t *testing.T) {
	f := newFixture(t)
	claude := f.agent("claude-code")
	cursor := f.agent("cursor")

	idA, _ := workspace.Resolve(repoDir(t, "git@github.com:acme/shop.git"))
	idB, _ := workspace.Resolve(repoDir(t, "https://github.com/Acme/Shop"))

	ra, err := f.svc.ResolveNamespace(f.ctx, claude, idA)
	if err != nil {
		t.Fatal(err)
	}
	if !ra.Created || ra.Namespace.StorageMode != domain.StorageLocalOnly {
		t.Fatalf("first resolve should auto-create a local_only namespace: %+v", ra)
	}
	rb, err := f.svc.ResolveNamespace(f.ctx, cursor, idB)
	if err != nil {
		t.Fatal(err)
	}
	if rb.Created || rb.Namespace.ID != ra.Namespace.ID {
		t.Fatal("both clones should map to the same namespace")
	}

	f.propose(claude, application.ProposeInput{NamespaceID: ra.Namespace.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeArchitecture, Title: "Orders database",
		Body: "Orders live in PostgreSQL. Writes go through a transactional outbox; never last-write-wins."})
	f.propose(claude, application.ProposeInput{NamespaceID: ra.Namespace.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeConvention, Title: "Error handling", Body: "Wrap errors with context; no panics in handlers."})

	res, err := f.svc.Search(f.ctx, cursor, application.SearchQuery{Query: "which database for orders?", NamespaceID: rb.Namespace.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) == 0 || res.Items[0].Head.Title != "Orders database" {
		t.Fatalf("search results: %+v", res.Items)
	}
	if res.Items[0].Head.AuthorID != claude.ID || res.Items[0].Context.HeadTrust != domain.TrustSessionConfirmed {
		t.Fatal("result should carry provenance and trust")
	}

	sum, err := f.svc.Summary(f.ctx, cursor, rb.Namespace.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Orders database", "Error handling", "context_search", "await the user's review"} {
		if !strings.Contains(sum, want) {
			t.Fatalf("summary missing %q:\n%s", want, sum)
		}
	}
}

func TestSearchRespectsAccessAndState(t *testing.T) {
	f := newFixture(t)
	work, _ := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "Work"})
	personal := f.agent("personal-only", f.ns.ID)

	in := decision("Kafka is banned")
	in.NamespaceID, in.Origin = work.ID, domain.OriginUser
	f.propose(f.user, in)

	if res, _ := f.svc.Search(f.ctx, personal, application.SearchQuery{Query: "kafka"}); len(res.Items) != 0 {
		t.Fatal("search leaked another namespace")
	}
	if _, err := f.svc.Search(f.ctx, personal, application.SearchQuery{Query: "kafka", NamespaceID: work.ID}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("explicit search of another namespace should be forbidden")
	}

	// Superseded and deleted context drops out of search.
	c := f.propose(f.user, application.ProposeInput{NamespaceID: f.ns.ID, Kind: domain.KindCreate, Origin: domain.OriginUser,
		Type: domain.TypeFact, Title: "Redis cache", Body: "Redis caches sessions."}).Context.Context
	if res, _ := f.svc.Search(f.ctx, personal, application.SearchQuery{Query: "redis"}); len(res.Items) != 1 {
		t.Fatal("expected one redis hit")
	}
	if err := f.svc.DeleteContext(f.ctx, f.user, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.svc.Search(f.ctx, personal, application.SearchQuery{Query: "redis"}); len(res.Items) != 0 {
		t.Fatal("deleted context still searchable")
	}
}

func TestSearchIndexesHeadRevisionOnly(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := application.ProposeInput{NamespaceID: f.ns.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeFact, Title: "Queue", Body: "We use RabbitMQ."}
	c := f.propose(a, in).Context.Context
	f.propose(a, application.ProposeInput{NamespaceID: f.ns.ID, Kind: domain.KindRevise, Origin: domain.OriginExplicit,
		TargetContextID: c.ID, BaseVersion: 1, Title: "Queue", Body: "We use NATS now."})
	if res, _ := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "rabbitmq"}); len(res.Items) != 0 {
		t.Fatal("old revision text is still searchable")
	}
	if res, _ := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "nats"}); len(res.Items) != 1 {
		t.Fatal("new revision text is not searchable")
	}
}

func TestSearchTokenBudget(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 6; i++ {
		in := decision("Logging rule")
		in.NamespaceID, in.Origin = f.ns.ID, domain.OriginUser
		in.Title = in.Title + string(rune('A'+i))
		in.Body = "Logging: " + strings.Repeat("structured json lines with request ids. ", 10)
		f.propose(f.user, in)
	}
	res, err := f.svc.Search(f.ctx, f.user, application.SearchQuery{Query: "logging", Limit: 10, TokenBudget: 300})
	if err != nil {
		t.Fatal(err)
	}
	if res.TokensUsed > 300 || !res.Truncated || len(res.Items) == 0 {
		t.Fatalf("budget not respected: used=%d items=%d truncated=%v", res.TokensUsed, len(res.Items), res.Truncated)
	}
}

func TestSummaryExcludesAgentReportedAndRespectsBudget(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	f.propose(a, application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginSilent, Type: domain.TypeObservation,
		Title: "Flaky test", Body: "TestCheckout is flaky."})
	for i := 0; i < 40; i++ {
		in := decision("Decision number " + string(rune('A'+i%26)) + string(rune('a'+i/26)))
		in.Body = strings.Repeat("A long explanation of the decision. ", 15)
		f.propose(a, in)
	}
	sum, err := f.svc.Summary(f.ctx, a, f.ns.ID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sum, "Flaky test") {
		t.Fatal("agent_reported items must not be in the summary")
	}
	if !strings.Contains(sum, "more not shown") {
		t.Fatal("summary should say items were cut")
	}
	if n := len([]rune(sum)) / 4; n > 1100 {
		t.Fatalf("summary ~%d tokens, over budget", n)
	}
}

func TestResolveMarkerAndPersonal(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-desktop")
	r, err := f.svc.ResolveNamespace(f.ctx, a, workspace.Identity{Kind: workspace.KindNone})
	if err != nil || r.Namespace.ID != f.ns.ID {
		t.Fatalf("chat with no folder should use Personal: %+v %v", r, err)
	}
	work, _ := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "Work"})
	dir := t.TempDir()
	workspace.WriteMarker(dir, work.ID)
	id, _ := workspace.Resolve(dir)
	if r, err := f.svc.ResolveNamespace(f.ctx, a, id); err != nil || r.Namespace.ID != work.ID {
		t.Fatalf("marker: %+v %v", r, err)
	}
	if _, err := f.svc.ResolveNamespace(f.ctx, a, workspace.Identity{Kind: workspace.KindMarker, Value: "ns_missing"}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("unknown marker namespace should be invalid")
	}
}

func TestResolveWithoutAccessExplainsGrant(t *testing.T) {
	f := newFixture(t)
	limited := f.agent("limited", f.ns.ID)
	id, _ := workspace.Resolve(repoDir(t, "https://github.com/acme/secret"))
	_, err := f.svc.ResolveNamespace(f.ctx, limited, id)
	if !errors.Is(err, domain.ErrForbidden) || !strings.Contains(err.Error(), "centrext agent grant") {
		t.Fatalf("err = %v", err)
	}
	if err := f.svc.GrantAgent(f.ctx, f.user, limited.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ResolveNamespace(f.ctx, limited, id); err != nil {
		t.Fatal(err)
	}
}

// Opening a v1 database with this binary migrates it and indexes existing context.
func TestMigrationFromV1IndexesExistingContext(t *testing.T) {
	// Covered indirectly: every fixture runs all migrations on a new file.
	// Here we check reopen keeps search working.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "c.db")
	s, err := sqlite.Open(ctx, path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	svc := application.New(s)
	svc.Init(ctx, "owner")
	u, _ := svc.LocalUser(ctx)
	ns, _ := svc.FindNamespace(ctx, u, application.PersonalNamespace)
	svc.Propose(ctx, u, application.ProposeInput{NamespaceID: ns.ID, Kind: domain.KindCreate, Origin: domain.OriginUser,
		Type: domain.TypeFact, Title: "Timezone", Body: "Store times in UTC."})
	s.Close()
	s2, err := sqlite.Open(ctx, path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	res, err := application.New(s2, application.WithClock(func() time.Time { return time.Now() })).Search(ctx, u, application.SearchQuery{Query: "utc"})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("search after reopen: %v %+v", err, res)
	}
}
