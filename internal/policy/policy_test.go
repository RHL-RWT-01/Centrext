package policy

import (
	"testing"
	"time"

	"centrext/internal/domain"
)

func agentActor() domain.Actor {
	a := &domain.Agent{ID: "agt_1", Name: "cc", Client: "claude-code"}
	return domain.Actor{Kind: domain.ActorAgent, ID: a.ID, Agent: a}
}

func userActor() domain.Actor { return domain.Actor{Kind: domain.ActorUser, ID: "usr_1"} }

func TestTrustForOriginNeverUserVerifiedForAgents(t *testing.T) {
	for _, o := range []domain.ProposalOrigin{domain.OriginExplicit, domain.OriginSuggested, domain.OriginSilent} {
		if got := TrustForOrigin(o); got == domain.TrustUserVerified {
			t.Fatalf("origin %s produced user_verified", o)
		}
	}
	if TrustForOrigin(domain.OriginUser) != domain.TrustUserVerified {
		t.Fatal("user origin should be user_verified")
	}
}

func TestEvaluateProposal(t *testing.T) {
	verified := &domain.Context{Type: domain.TypeFact, HeadTrust: domain.TrustUserVerified}
	confirmedFact := &domain.Context{Type: domain.TypeFact, HeadTrust: domain.TrustSessionConfirmed}
	confirmedDecision := &domain.Context{Type: domain.TypeDecision, HeadTrust: domain.TrustSessionConfirmed}

	cases := []struct {
		name    string
		actor   domain.Actor
		req     ProposalRequest
		target  *domain.Context
		outcome Outcome
		trust   domain.Trust
		expires bool
	}{
		{"user create", userActor(), ProposalRequest{domain.KindCreate, domain.OriginUser, domain.TypeDecision}, nil, AutoAccept, domain.TrustUserVerified, false},
		{"user with agent origin denied", userActor(), ProposalRequest{domain.KindCreate, domain.OriginExplicit, domain.TypeFact}, nil, Deny, "", false},
		{"agent claims user origin", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginUser, domain.TypeFact}, nil, Deny, "", false},
		{"explicit create any type", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginExplicit, domain.TypeTask}, nil, AutoAccept, domain.TrustSessionConfirmed, false},
		{"suggested decision", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginSuggested, domain.TypeDecision}, nil, AutoAccept, domain.TrustSessionConfirmed, false},
		{"suggested task denied", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginSuggested, domain.TypeTask}, nil, Deny, "", false},
		{"silent observation", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginSilent, domain.TypeObservation}, nil, AutoAccept, domain.TrustAgentReported, true},
		{"silent decision denied", agentActor(), ProposalRequest{domain.KindCreate, domain.OriginSilent, domain.TypeDecision}, nil, Deny, "", false},
		{"revise user_verified needs approval", agentActor(), ProposalRequest{domain.KindRevise, domain.OriginExplicit, domain.TypeFact}, verified, NeedsApproval, domain.TrustSessionConfirmed, false},
		{"revise confirmed fact auto", agentActor(), ProposalRequest{domain.KindRevise, domain.OriginExplicit, domain.TypeFact}, confirmedFact, AutoAccept, domain.TrustSessionConfirmed, false},
		{"revise decision needs approval", agentActor(), ProposalRequest{domain.KindRevise, domain.OriginExplicit, domain.TypeDecision}, confirmedDecision, NeedsApproval, domain.TrustSessionConfirmed, false},
		{"silent revise denied", agentActor(), ProposalRequest{domain.KindRevise, domain.OriginSilent, domain.TypeObservation}, confirmedFact, Deny, "", false},
		{"supersede needs approval", agentActor(), ProposalRequest{domain.KindSupersede, domain.OriginExplicit, domain.TypeFact}, confirmedFact, NeedsApproval, domain.TrustSessionConfirmed, false},
		{"namespace needs approval", agentActor(), ProposalRequest{domain.KindCreateNamespace, domain.OriginSuggested, ""}, nil, NeedsApproval, domain.TrustSessionConfirmed, false},
		{"silent namespace denied", agentActor(), ProposalRequest{domain.KindCreateNamespace, domain.OriginSilent, ""}, nil, Deny, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := EvaluateProposal(c.actor, c.req, c.target)
			if d.Outcome != c.outcome {
				t.Fatalf("outcome = %s, want %s (%s)", d.Outcome, c.outcome, d.Reason)
			}
			if c.outcome != Deny && d.Trust != c.trust {
				t.Fatalf("trust = %s, want %s", d.Trust, c.trust)
			}
			if d.Expires != c.expires {
				t.Fatalf("expires = %v, want %v", d.Expires, c.expires)
			}
		})
	}
}

func TestRevokedAgentDenied(t *testing.T) {
	a := agentActor()
	now := time.Now()
	a.Agent.RevokedAt = &now
	if d := EvaluateProposal(a, ProposalRequest{domain.KindCreate, domain.OriginExplicit, domain.TypeFact}, nil); d.Outcome != Deny {
		t.Fatal("revoked agent should be denied")
	}
	if err := CanAccessNamespace(a, "ns", []domain.Grant{{AgentID: a.ID}}); err == nil {
		t.Fatal("revoked agent should not access namespaces")
	}
}

func TestCanAccessNamespace(t *testing.T) {
	a := agentActor()
	if err := CanAccessNamespace(a, "ns_a", nil); err == nil {
		t.Fatal("no grant should deny")
	}
	if err := CanAccessNamespace(a, "ns_a", []domain.Grant{{AgentID: a.ID, NamespaceID: "ns_b"}}); err == nil {
		t.Fatal("grant for another namespace should deny")
	}
	if err := CanAccessNamespace(a, "ns_a", []domain.Grant{{AgentID: a.ID, NamespaceID: "ns_a"}}); err != nil {
		t.Fatal(err)
	}
	if err := CanAccessNamespace(a, "ns_a", []domain.Grant{{AgentID: a.ID}}); err != nil {
		t.Fatal("all-namespaces grant should allow:", err)
	}
	if err := CanAccessNamespace(a, "ns_a", []domain.Grant{{AgentID: "other"}}); err == nil {
		t.Fatal("another agent's grant should deny")
	}
}

func TestDetectSecret(t *testing.T) {
	hits := []string{
		"key is AKIAIOSFODNN7EXAMPLE ok",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"token ghp_abcdefghijklmnopqrstuvwxyz0123456789AB",
		"OPENAI sk-proj-abcdefghijklmnopqrstuvwxyz",
		"password = hunter2hunter2",
		"postgres://app:s3cretpw@db.internal:5432/app",
	}
	for _, h := range hits {
		if DetectSecret(h) == "" {
			t.Errorf("missed secret in %q", h)
		}
	}
	misses := []string{
		"We use Postgres with an outbox table for sync.",
		"Store the API key in the OS keychain, never in config.",
		"Connect to postgres://localhost:5432/app",
		"The password policy requires rotation.",
	}
	for _, m := range misses {
		if s := DetectSecret(m); s != "" {
			t.Errorf("false positive %s in %q", s, m)
		}
	}
}
