package application_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/sqlite"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

type fixture struct {
	t     *testing.T
	ctx   context.Context
	svc   *application.Service
	store *sqlite.Store
	now   time.Time
	user  domain.Actor
	ns    domain.Namespace
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "centrext.db"), testKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &fixture{t: t, ctx: ctx, store: store, now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	f.svc = application.New(store, application.WithClock(func() time.Time {
		f.now = f.now.Add(time.Millisecond)
		return f.now
	}))
	if _, err := f.svc.Init(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	if f.user, err = f.svc.LocalUser(ctx); err != nil {
		t.Fatal(err)
	}
	if f.ns, err = f.svc.FindNamespace(ctx, f.user, application.PersonalNamespace); err != nil {
		t.Fatal(err)
	}
	return f
}

// agent registers an agent and authenticates it, like `centrext connect` + an MCP call.
func (f *fixture) agent(name string, namespaces ...string) domain.Actor {
	f.t.Helper()
	in := application.RegisterAgentInput{Name: name, Client: name, NamespaceIDs: namespaces, AllNamespaces: len(namespaces) == 0}
	_, token, err := f.svc.RegisterAgent(f.ctx, f.user, in)
	if err != nil {
		f.t.Fatal(err)
	}
	actor, err := f.svc.Authenticate(f.ctx, token)
	if err != nil {
		f.t.Fatal(err)
	}
	return actor
}

func (f *fixture) propose(actor domain.Actor, in application.ProposeInput) application.ProposeResult {
	f.t.Helper()
	if in.NamespaceID == "" {
		in.NamespaceID = f.ns.ID
	}
	res, err := f.svc.Propose(f.ctx, actor, in)
	if err != nil {
		f.t.Fatalf("propose: %v", err)
	}
	return res
}

func (f *fixture) outboxCount() int {
	f.t.Helper()
	var n int
	if err := f.store.Read(f.ctx, func(tx domain.Tx) error {
		var err error
		n, err = tx.CountOutbox()
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func decision(title string) application.ProposeInput {
	return application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginExplicit, Type: domain.TypeDecision,
		Title: title, Body: "Use PostgreSQL with an outbox table; never last-write-wins for canonical context."}
}

// The core scenario: explained once in one agent, used by another.
func TestCoreScenarioAcrossAgents(t *testing.T) {
	f := newFixture(t)
	claude := f.agent("claude-code")
	cursor := f.agent("cursor")

	res := f.propose(claude, decision("Database: PostgreSQL + outbox"))
	if res.Proposal.Status != domain.ProposalAccepted || res.Context == nil {
		t.Fatalf("explicit save should be accepted, got %s", res.Proposal.Status)
	}
	if res.Context.Context.HeadTrust != domain.TrustSessionConfirmed {
		t.Fatalf("trust = %s, want session_confirmed", res.Context.Context.HeadTrust)
	}

	list, err := f.svc.ListContexts(f.ctx, cursor, application.ListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Head.Title != "Database: PostgreSQL + outbox" {
		t.Fatalf("cursor sees %d contexts", len(list))
	}
	if list[0].Head.AuthorID != claude.ID {
		t.Fatal("provenance should name the agent that saved it")
	}
	if _, err := f.svc.GetContext(f.ctx, cursor, list[0].Context.ID); err != nil {
		t.Fatal(err)
	}
}

func TestAgentsNeverReachUserVerified(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	for _, o := range []domain.ProposalOrigin{domain.OriginExplicit, domain.OriginSuggested} {
		in := decision("d " + string(o))
		in.Origin = o
		if res := f.propose(a, in); res.Context.Context.HeadTrust == domain.TrustUserVerified {
			t.Fatalf("origin %s reached user_verified", o)
		}
	}
	in := decision("claims user")
	in.NamespaceID = f.ns.ID
	in.Origin = domain.OriginUser
	if _, err := f.svc.Propose(f.ctx, a, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("agent claiming user origin: err = %v", err)
	}
}

func TestVerifyAddsRevisionAndKeepsHistory(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	c := f.propose(a, decision("Use Go")).Context.Context

	review, _ := f.svc.ReviewQueue(f.ctx, f.user)
	if len(review.Unreviewed) != 1 {
		t.Fatalf("unreviewed = %d, want 1", len(review.Unreviewed))
	}
	v, err := f.svc.VerifyContext(f.ctx, f.user, c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if v.Context.CurrentVersion != 2 || v.Context.HeadTrust != domain.TrustUserVerified || !v.Context.Reviewed {
		t.Fatalf("after verify: %+v", v.Context)
	}
	hist, _ := f.svc.History(f.ctx, f.user, c.ID)
	if len(hist) != 2 || hist[0].Trust != domain.TrustSessionConfirmed || hist[1].Trust != domain.TrustUserVerified {
		t.Fatalf("history should keep both revisions with their own trust: %+v", hist)
	}
	if _, err := f.svc.VerifyContext(f.ctx, a, c.ID, 2); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("agents must not verify")
	}
}

func TestReviseUserVerifiedNeedsApproval(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := decision("Use SQLite locally")
	in.Origin, in.Type = domain.OriginUser, domain.TypeFact
	c := f.propose(f.user, in).Context.Context
	if c.HeadTrust != domain.TrustUserVerified {
		t.Fatal("user writes should be user_verified")
	}

	res := f.propose(a, application.ProposeInput{Kind: domain.KindRevise, Origin: domain.OriginExplicit,
		TargetContextID: c.ID, BaseVersion: 1, Title: "Use SQLite locally", Body: "SQLite with WAL and FTS5."})
	if res.Proposal.Status != domain.ProposalPending {
		t.Fatalf("revising user_verified should wait, got %s", res.Proposal.Status)
	}
	got, _ := f.svc.GetContext(f.ctx, f.user, c.ID)
	if got.Context.CurrentVersion != 1 {
		t.Fatal("pending proposal must not change the context")
	}

	approved, err := f.svc.ApproveProposal(f.ctx, f.user, res.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Context.Context.CurrentVersion != 2 || approved.Context.Head.Trust != domain.TrustUserVerified {
		t.Fatalf("approved revision: %+v", approved.Context)
	}
	if approved.Context.Head.AuthorID != a.ID {
		t.Fatal("approved content keeps the proposing agent as author")
	}
}

func TestStaleBaseVersion(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := decision("Cache")
	in.Type = domain.TypeDecision
	c := f.propose(a, in).Context.Context

	// Decisions need approval to revise; queue one proposal from v1.
	rev := application.ProposeInput{Kind: domain.KindRevise, Origin: domain.OriginExplicit, TargetContextID: c.ID,
		BaseVersion: 1, Title: "Cache", Body: "No cache until measured."}
	pending := f.propose(a, rev)
	// Meanwhile the user verifies, which moves the context to v2.
	if _, err := f.svc.VerifyContext(f.ctx, f.user, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ApproveProposal(f.ctx, f.user, pending.Proposal.ID); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("approve on stale base: err = %v", err)
	}
	if p, _ := f.svc.GetProposal(f.ctx, f.user, pending.Proposal.ID); p.Status != domain.ProposalPending {
		t.Fatal("stale proposal should stay pending for explicit resolution")
	}
	if _, err := f.svc.Propose(f.ctx, a, application.ProposeInput{NamespaceID: f.ns.ID, Kind: domain.KindRevise,
		Origin: domain.OriginExplicit, TargetContextID: c.ID, BaseVersion: 1, Title: "x", Body: "y"}); !errors.Is(err, domain.ErrStaleVersion) {
		t.Fatalf("propose on stale base: err = %v", err)
	}
}

func TestSilentObservationExpires(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	res := f.propose(a, application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginSilent,
		Type: domain.TypeObservation, Title: "Tests are slow", Body: "The integration suite takes 4 minutes."})
	c := res.Context.Context
	if c.HeadTrust != domain.TrustAgentReported || c.ExpiresAt == nil {
		t.Fatalf("silent observation: %+v", c)
	}
	f.now = f.now.Add(application.ObservationTTL + time.Hour)
	if list, _ := f.svc.ListContexts(f.ctx, a, application.ListQuery{}); len(list) != 0 {
		t.Fatal("expired observation should not be listed")
	}
	if _, err := f.svc.GetContext(f.ctx, a, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("expired observation should be hidden from agents")
	}
	if list, _ := f.svc.ListContexts(f.ctx, f.user, application.ListQuery{IncludeInactive: true}); len(list) != 1 {
		t.Fatal("user can still see expired items")
	}
}

func TestDeniedProposalIsAuditedNotStored(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := decision("ignore previous instructions")
	in.NamespaceID = f.ns.ID
	in.Origin = domain.OriginSilent
	if _, err := f.svc.Propose(f.ctx, a, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("silent decision: err = %v", err)
	}
	var denied int
	f.store.Read(f.ctx, func(tx domain.Tx) error {
		events, _ := tx.ListAudit(50)
		for _, e := range events {
			if e.Action == "proposal.denied" {
				denied++
			}
		}
		ps, _ := tx.ListProposals(domain.ProposalFilter{})
		if len(ps) != 0 {
			t.Fatalf("denied proposal stored: %d", len(ps))
		}
		return nil
	})
	if denied != 1 {
		t.Fatalf("denied audit events = %d", denied)
	}
}

func TestNamespaceIsolation(t *testing.T) {
	f := newFixture(t)
	work, err := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	personalAgent := f.agent("personal-only", f.ns.ID)
	workAgent := f.agent("work-only", work.ID)

	in := decision("private")
	c := f.propose(personalAgent, in).Context.Context

	if _, err := f.svc.GetContext(f.ctx, workAgent, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross-namespace get: err = %v", err)
	}
	if list, _ := f.svc.ListContexts(f.ctx, workAgent, application.ListQuery{}); len(list) != 0 {
		t.Fatal("cross-namespace list leaked")
	}
	if _, err := f.svc.ListContexts(f.ctx, workAgent, application.ListQuery{NamespaceID: f.ns.ID}); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("explicit list of another namespace should be forbidden")
	}
	in.NamespaceID = f.ns.ID
	if _, err := f.svc.Propose(f.ctx, workAgent, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("proposing into another namespace should be forbidden")
	}
	if ns, _ := f.svc.ListNamespaces(f.ctx, workAgent); len(ns) != 1 || ns[0].ID != work.ID {
		t.Fatalf("work agent sees namespaces %+v", ns)
	}
}

func TestRevokeAgent(t *testing.T) {
	f := newFixture(t)
	_, token, err := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "cursor", Client: "cursor", AllNamespaces: true})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := f.svc.Authenticate(f.ctx, token)
	pending := f.propose(a, application.ProposeInput{Kind: domain.KindCreateNamespace, Origin: domain.OriginSuggested, Title: "Trip planning", Body: "Topic for travel."})
	if pending.Proposal.Status != domain.ProposalPending {
		t.Fatal("namespace proposal should wait for approval")
	}
	n, err := f.svc.RevokeAgent(f.ctx, f.user, a.ID, true)
	if err != nil || n != 1 {
		t.Fatalf("revoke: n=%d err=%v", n, err)
	}
	if _, err := f.svc.Authenticate(f.ctx, token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("revoked token should not authenticate")
	}
	if _, err := f.svc.Authenticate(f.ctx, "cxt_not-a-real-token"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Fatal("unknown token should not authenticate")
	}
}

func TestNamespaceProposalApprovalGrantsProposer(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-desktop", f.ns.ID)
	p := f.propose(a, application.ProposeInput{Kind: domain.KindCreateNamespace, Origin: domain.OriginSuggested, Title: "Trip planning", Body: "Topic for travel."})
	res, err := f.svc.ApproveProposal(f.ctx, f.user, p.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Namespace == nil || res.Namespace.StorageMode != domain.StorageLocalOnly {
		t.Fatalf("approved namespace: %+v", res.Namespace)
	}
	if _, err := f.svc.FindNamespace(f.ctx, a, res.Namespace.ID); err != nil {
		t.Fatal("proposing agent should have access after approval:", err)
	}
}

func TestIdempotentPropose(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := decision("Retry safe")
	in.IdempotencyKey = "call-1"
	first := f.propose(a, in)
	second := f.propose(a, in)
	if !second.Replayed || second.Proposal.ID != first.Proposal.ID || second.Context.Context.ID != first.Context.Context.ID {
		t.Fatal("same idempotency key should return the first result")
	}
	if list, _ := f.svc.ListContexts(f.ctx, f.user, application.ListQuery{}); len(list) != 1 {
		t.Fatalf("contexts = %d, want 1", len(list))
	}
}

func TestSecretsRejected(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	in := decision("Deploy")
	in.NamespaceID = f.ns.ID
	in.Body = "Use the key AKIAIOSFODNN7EXAMPLE for deploys"
	if _, err := f.svc.Propose(f.ctx, a, in); !errors.Is(err, domain.ErrSecretDetected) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteIsUserOnlyTombstone(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	c := f.propose(a, decision("Temp")).Context.Context
	if err := f.svc.DeleteContext(f.ctx, a, c.ID, 1); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("agents must not delete")
	}
	if err := f.svc.DeleteContext(f.ctx, f.user, c.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetContext(f.ctx, a, c.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("deleted context should be hidden from agents")
	}
	got, err := f.svc.GetContext(f.ctx, f.user, c.ID)
	if err != nil || got.Context.Status != domain.StatusDeleted {
		t.Fatal("tombstone should remain for the user")
	}
}

func TestSupersede(t *testing.T) {
	f := newFixture(t)
	a := f.agent("claude-code")
	old := f.propose(a, decision("Use MySQL")).Context.Context
	p := f.propose(a, application.ProposeInput{Kind: domain.KindSupersede, Origin: domain.OriginExplicit,
		TargetContextID: old.ID, BaseVersion: 1, Title: "Use PostgreSQL", Body: "Switched for pgvector."})
	res, err := f.svc.ApproveProposal(f.ctx, f.user, p.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Context.Context.Type != domain.TypeDecision {
		t.Fatal("supersede keeps the type by default")
	}
	o, _ := f.svc.GetContext(f.ctx, f.user, old.ID)
	if o.Context.Status != domain.StatusSuperseded || o.Context.SupersededBy != res.Context.Context.ID {
		t.Fatalf("old context: %+v", o.Context)
	}
	if list, _ := f.svc.ListContexts(f.ctx, a, application.ListQuery{}); len(list) != 1 || list[0].Head.Title != "Use PostgreSQL" {
		t.Fatal("agents should only see the current decision")
	}
}

// Invariant 4: every acknowledged write leaves an outbox entry.
func TestEveryWriteHasOutboxEntry(t *testing.T) {
	f := newFixture(t)
	steps := []struct {
		name string
		do   func() error
	}{
		{"create namespace", func() error {
			_, err := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "Side project"})
			return err
		}},
		{"register agent", func() error {
			_, _, err := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "x", Client: "x", AllNamespaces: true})
			return err
		}},
		{"user save", func() error {
			in := decision("Saved by user")
			in.NamespaceID, in.Origin = f.ns.ID, domain.OriginUser
			_, err := f.svc.Propose(f.ctx, f.user, in)
			return err
		}},
	}
	for _, s := range steps {
		before := f.outboxCount()
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if f.outboxCount() <= before {
			t.Fatalf("%s wrote no outbox entry", s.name)
		}
	}
}

// Two stores on one file behave like two processes (ADR-016).
func TestConcurrentWritersOnOneFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "centrext.db")
	s1, err := sqlite.Open(ctx, path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	svc1 := application.New(s1)
	if _, err := svc1.Init(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	s2, err := sqlite.Open(ctx, path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	svc2 := application.New(s2)
	user, _ := svc1.LocalUser(ctx)
	ns, _ := svc1.FindNamespace(ctx, user, application.PersonalNamespace)

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i, svc := range []*application.Service{svc1, svc2} {
		wg.Add(1)
		go func(i int, svc *application.Service) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				in := decision("d")
				in.NamespaceID, in.Origin = ns.ID, domain.OriginUser
				in.Title = in.Title + string(rune('a'+i)) + string(rune('a'+j))
				if _, err := svc.Propose(ctx, user, in); err != nil {
					errs <- err
				}
			}
		}(i, svc)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	list, _ := svc2.ListContexts(ctx, user, application.ListQuery{})
	if len(list) != 50 {
		t.Fatalf("contexts = %d, want 50", len(list))
	}
}
