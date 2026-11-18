package application_test

import (
	"errors"
	"testing"

	"centrext/internal/application"
	"centrext/internal/domain"
)

func TestMarkUsedRespectsAccess(t *testing.T) {
	f := newFixture(t)
	work, _ := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: "Work"})
	in := decision("Secret plan")
	in.NamespaceID, in.Origin = work.ID, domain.OriginUser
	hidden := f.propose(f.user, in).Context.Context

	limited := f.agent("limited", f.ns.ID)
	res, err := f.svc.MarkUsed(f.ctx, limited, []string{hidden.ID, hidden.ID})
	if err != nil || len(res.Recorded) != 0 || len(res.Unknown) != 1 {
		t.Fatalf("an agent must not record use of context it can't read: %+v %v", res, err)
	}
	if _, err := f.svc.MarkUsed(f.ctx, f.user, []string{hidden.ID}); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("users don't report usage")
	}
	many := make([]string, application.MaxUsedPerCall+1)
	if _, err := f.svc.MarkUsed(f.ctx, limited, many); !errors.Is(err, domain.ErrInvalid) {
		t.Fatal("too many ids should be refused")
	}
	if _, err := f.svc.CaptureStats(f.ctx, limited); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("agents can't read metrics")
	}
}
