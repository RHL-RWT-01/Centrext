package application_test

import (
	"testing"

	"centrext/internal/application"
	"centrext/internal/domain"
)

func TestPromptContextGateTrustAndExclusions(t *testing.T) {
	f := newFixture(t)
	claude := f.agent("claude-code")
	db := f.propose(claude, application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginExplicit, Type: domain.TypeArchitecture,
		Title: "Orders database", Body: "Orders live in PostgreSQL behind a transactional outbox."}).Context.Context
	f.propose(claude, application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginSilent, Type: domain.TypeObservation,
		Title: "Orders migration flaky", Body: "The orders migration in PostgreSQL timed out once."})

	ask := func(actor domain.Actor, prompt string, exclude ...string) []string {
		t.Helper()
		res, err := f.svc.PromptContext(f.ctx, actor, application.PromptQuery{Prompt: prompt, NamespaceID: f.ns.ID, Exclude: exclude})
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, it := range res.Items {
			ids = append(ids, it.Context.ID)
		}
		return ids
	}
	if got := ask(claude, "write a migration for the orders table in postgresql"); len(got) != 1 || got[0] != db.ID {
		t.Fatalf("expected the architecture item only (agent_reported is never injected): %v", got)
	}
	if got := ask(claude, "can you order these functions alphabetically"); len(got) != 0 {
		t.Fatalf("one incidental word must not inject anything: %v", got)
	}
	if got := ask(claude, "write a migration for the orders table in postgresql", db.ID); len(got) != 0 {
		t.Fatalf("items already delivered this session must be skipped: %v", got)
	}
	elsewhere, err := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	other := f.agent("cursor", elsewhere.ID)
	if _, err := f.svc.PromptContext(f.ctx, other, application.PromptQuery{Prompt: "orders postgresql", NamespaceID: f.ns.ID}); err == nil {
		t.Fatal("an agent without access must be refused")
	}
}
