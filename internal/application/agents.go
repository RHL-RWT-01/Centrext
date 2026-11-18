package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"centrext/internal/domain"
	"centrext/internal/policy"
)

// TokenPrefix marks Centrext agent tokens so they're easy to recognize.
const TokenPrefix = "cxt_"

// RegisterAgentInput is the input to RegisterAgent.
type RegisterAgentInput struct {
	Name          string
	Client        string   // e.g. claude-code, cursor, claude-desktop
	NamespaceIDs  []string // explicit grants
	AllNamespaces bool     // grant every namespace, present and future
}

// RegisterAgent creates an agent and returns its token. The token is shown
// once; only its hash is stored (ADR-018).
func (s *Service) RegisterAgent(ctx context.Context, actor domain.Actor, in RegisterAgentInput) (domain.Agent, string, error) {
	if err := policy.RequireUser(actor, "registering an agent"); err != nil {
		return domain.Agent{}, "", err
	}
	if err := domain.ValidateName(in.Name); err != nil {
		return domain.Agent{}, "", err
	}
	if err := domain.ValidateName(in.Client); err != nil {
		return domain.Agent{}, "", domain.Invalidf("client: %v", err)
	}
	if !in.AllNamespaces && len(in.NamespaceIDs) == 0 {
		return domain.Agent{}, "", domain.Invalidf("grant at least one namespace, or all namespaces")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return domain.Agent{}, "", err
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])

	var agent domain.Agent
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		now := s.now()
		agent = domain.Agent{ID: newID("agt", now), Name: strings.TrimSpace(in.Name), Client: strings.TrimSpace(in.Client),
			TokenHash: hashToken(token), CreatedAt: now}
		if err := tx.InsertAgent(agent); err != nil {
			return err
		}
		grants := in.NamespaceIDs
		if in.AllNamespaces {
			grants = []string{""}
		}
		for _, nsID := range grants {
			if nsID != "" {
				if _, err := tx.GetNamespace(nsID); err != nil {
					return err
				}
			}
			if err := tx.InsertGrant(domain.Grant{AgentID: agent.ID, NamespaceID: nsID}); err != nil {
				return err
			}
		}
		// The token hash stays local: it is not part of the replicated payload.
		return s.record(tx, actor, "agent.registered", "agent", agent.ID, map[string]any{
			"name": agent.Name, "client": agent.Client, "namespaces": grants,
		})
	})
	if err != nil {
		return domain.Agent{}, "", err
	}
	return agent, token, nil
}

// Authenticate turns a token into an agent actor. Revoked or unknown tokens
// are rejected (invariant 1).
func (s *Service) Authenticate(ctx context.Context, token string) (domain.Actor, error) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return domain.Actor{}, domain.ErrUnauthenticated
	}
	var actor domain.Actor
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		a, err := tx.GetAgentByTokenHash(hashToken(token))
		if err == domain.ErrNotFound {
			return domain.ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if !a.Active() {
			return domain.ErrUnauthenticated
		}
		now := s.now()
		a.LastSeenAt = &now
		if err := tx.UpdateAgent(a); err != nil {
			return err
		}
		actor = domain.Actor{Kind: domain.ActorAgent, ID: a.ID, Agent: &a}
		return nil
	})
	return actor, err
}

// RevokeAgent blocks an agent. Past writes keep their attribution. When
// rejectPending is set, the agent's pending proposals are rejected too.
func (s *Service) RevokeAgent(ctx context.Context, actor domain.Actor, agentID string, rejectPending bool) (int, error) {
	if err := policy.RequireUser(actor, "revoking an agent"); err != nil {
		return 0, err
	}
	rejected := 0
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if a.RevokedAt == nil {
			now := s.now()
			a.RevokedAt = &now
			if err := tx.UpdateAgent(a); err != nil {
				return err
			}
			if err := s.record(tx, actor, "agent.revoked", "agent", a.ID, map[string]any{"name": a.Name}); err != nil {
				return err
			}
		}
		if !rejectPending {
			return nil
		}
		pending, err := tx.ListProposals(domain.ProposalFilter{Statuses: []domain.ProposalStatus{domain.ProposalPending}, AgentID: a.ID})
		if err != nil {
			return err
		}
		for _, p := range pending {
			if err := s.decide(tx, actor, p, domain.ProposalRejected, "agent revoked"); err != nil {
				return err
			}
			rejected++
		}
		return nil
	})
	return rejected, err
}

// ListAgents lists registered agents.
func (s *Service) ListAgents(ctx context.Context, actor domain.Actor) ([]domain.Agent, error) {
	if err := policy.RequireUser(actor, "listing agents"); err != nil {
		return nil, err
	}
	var out []domain.Agent
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		out, err = tx.ListAgents()
		return err
	})
	return out, err
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ActAsAgent lets the local user read as one of their agents, with that
// agent's grants, without its token. Local delivery code uses it to build
// copies an agent will see (the Cursor rules file), so a copy never shows more
// than the agent itself could read. Revoked agents are refused.
func (s *Service) ActAsAgent(ctx context.Context, actor domain.Actor, agentID string) (domain.Actor, error) {
	if err := policy.RequireUser(actor, "acting as an agent"); err != nil {
		return domain.Actor{}, err
	}
	var out domain.Actor
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		a, err := tx.GetAgent(agentID)
		if err != nil {
			return err
		}
		if !a.Active() {
			return domain.Forbiddenf("agent %s is revoked", agentID)
		}
		out = domain.Actor{Kind: domain.ActorAgent, ID: a.ID, Agent: &a}
		return nil
	})
	return out, err
}

// GetAgent returns one agent.
func (s *Service) GetAgent(ctx context.Context, actor domain.Actor, agentID string) (domain.Agent, error) {
	if err := policy.RequireUser(actor, "reading agents"); err != nil {
		return domain.Agent{}, err
	}
	var out domain.Agent
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		out, err = tx.GetAgent(agentID)
		return err
	})
	return out, err
}

// AgentForToken returns the agent a token belongs to, revoked or not, without
// recording a use. `centrext doctor` uses it to check stored tokens and to
// name the agent behind a token found in a repo.
func (s *Service) AgentForToken(ctx context.Context, actor domain.Actor, token string) (domain.Agent, error) {
	if err := policy.RequireUser(actor, "checking tokens"); err != nil {
		return domain.Agent{}, err
	}
	var out domain.Agent
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		out, err = tx.GetAgentByTokenHash(hashToken(token))
		return err
	})
	return out, err
}
