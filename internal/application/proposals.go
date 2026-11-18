package application

import (
	"context"
	"fmt"
	"strings"
	"time"

	"centrext/internal/domain"
	"centrext/internal/policy"
)

// ProposeInput is a request to create, revise or supersede context, or to
// create a namespace.
type ProposeInput struct {
	NamespaceID     string
	Kind            domain.ProposalKind
	Origin          domain.ProposalOrigin
	TargetContextID string // revise / supersede
	BaseVersion     int    // revise / supersede
	Type            domain.ContextType
	Title           string // the namespace name for create_namespace
	Body            string
	IdempotencyKey  string // agents: repeat calls with the same key return the first result
}

// ProposeResult says what happened to a proposal.
type ProposeResult struct {
	Proposal  domain.Proposal
	Context   *ContextView      // set when the proposal was accepted into a context
	Namespace *domain.Namespace // set when a create_namespace proposal was accepted
	Replayed  bool              // an earlier call with the same idempotency key
}

// Propose is the single write path for context (ADR-007). Policy decides
// whether the proposal is accepted now, waits for the user, or is refused.
// Trust is assigned here from the origin, never taken from the caller.
func (s *Service) Propose(ctx context.Context, actor domain.Actor, in ProposeInput) (ProposeResult, error) {
	in.Title, in.Body = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body)
	if err := validateProposal(in); err != nil {
		return ProposeResult{}, err
	}
	if name := policy.DetectSecret(in.Title, in.Body); name != "" {
		return ProposeResult{}, fmt.Errorf("%w (%s); remove it and try again", domain.ErrSecretDetected, name)
	}

	var res ProposeResult
	var denied error
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		if !actor.IsUser() && in.IdempotencyKey != "" {
			p, err := tx.GetProposalByIdempotencyKey(actor.ID, in.IdempotencyKey)
			if err == nil {
				res = ProposeResult{Proposal: p, Replayed: true}
				return s.attachResult(tx, &res)
			}
			if err != domain.ErrNotFound {
				return err
			}
		}

		if _, err := tx.GetNamespace(in.NamespaceID); err != nil {
			return err
		}
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		if err := policy.CanAccessNamespace(actor, in.NamespaceID, grants); err != nil {
			return err
		}

		var target *domain.Context
		if in.Kind == domain.KindRevise || in.Kind == domain.KindSupersede {
			c, err := s.authorizeContext(tx, actor, in.TargetContextID)
			if err != nil {
				return err
			}
			if c.NamespaceID != in.NamespaceID {
				return domain.Invalidf("target context is in another namespace")
			}
			if c.Status != domain.StatusActive {
				return domain.Invalidf("target context is %s", c.Status)
			}
			if c.CurrentVersion != in.BaseVersion {
				return domain.ErrStaleVersion
			}
			if in.Kind == domain.KindRevise || in.Type == "" {
				in.Type = c.Type
			}
			target = &c
		}

		decision := policy.EvaluateProposal(actor, policy.ProposalRequest{Kind: in.Kind, Origin: in.Origin, Type: in.Type}, target)
		if decision.Outcome == policy.Deny {
			denied = domain.Forbiddenf("%s", decision.Reason)
			// Keep a record of refused writes (e.g. a prompt-injected agent), then stop.
			return s.audit(tx, actor, "proposal.denied", "namespace:"+in.NamespaceID, map[string]any{
				"kind": in.Kind, "origin": in.Origin, "type": in.Type, "reason": decision.Reason,
			})
		}

		now := s.now()
		p := domain.Proposal{
			ID: newID("prp", now), NamespaceID: in.NamespaceID, Kind: in.Kind, Origin: in.Origin,
			Status: domain.ProposalPending, TargetContextID: in.TargetContextID, BaseVersion: in.BaseVersion,
			Type: in.Type, Title: in.Title, Body: in.Body, Trust: decision.Trust,
			IdempotencyKey: in.IdempotencyKey, CreatedAt: now,
		}
		if !actor.IsUser() {
			p.AgentID = actor.ID
		}
		if err := tx.InsertProposal(p); err != nil {
			return err
		}
		if err := s.record(tx, actor, "proposal.created", "proposal", p.ID, proposalPayload(p, decision)); err != nil {
			return err
		}

		res.Proposal = p
		if decision.Outcome == policy.NeedsApproval {
			return nil
		}
		if err := s.apply(tx, actor, &p, decision.Trust, decision.Expires); err != nil {
			return err
		}
		res.Proposal = p
		return s.attachResult(tx, &res)
	})
	if err != nil {
		return ProposeResult{}, err
	}
	if denied != nil {
		return ProposeResult{}, denied
	}
	if !res.Replayed {
		s.changed(ctx, res.Proposal.NamespaceID)
	}
	return res, nil
}

func validateProposal(in ProposeInput) error {
	if !in.Kind.Valid() {
		return domain.Invalidf("unknown proposal kind %q", in.Kind)
	}
	if !in.Origin.Valid() {
		return domain.Invalidf("unknown origin %q", in.Origin)
	}
	if in.NamespaceID == "" {
		return domain.Invalidf("namespace is required")
	}
	switch in.Kind {
	case domain.KindCreateNamespace:
		return domain.ValidateName(in.Title)
	case domain.KindCreate:
		if !in.Type.Valid() {
			return domain.Invalidf("unknown context type %q", in.Type)
		}
	case domain.KindRevise, domain.KindSupersede:
		if in.TargetContextID == "" {
			return domain.Invalidf("target context is required")
		}
		if in.BaseVersion < 1 {
			return domain.Invalidf("base_version is required")
		}
		if in.Type != "" && !in.Type.Valid() {
			return domain.Invalidf("unknown context type %q", in.Type)
		}
	}
	return domain.ValidateContent(in.Title, in.Body)
}

func proposalPayload(p domain.Proposal, d policy.Decision) map[string]any {
	return map[string]any{
		"kind": p.Kind, "origin": p.Origin, "namespace_id": p.NamespaceID, "type": p.Type,
		"target_context_id": p.TargetContextID, "base_version": p.BaseVersion,
		"title": p.Title, "body": p.Body, "trust": p.Trust, "agent_id": p.AgentID,
		"outcome": d.Outcome.String(),
	}
}

// attachResult fills in the context or namespace an accepted proposal produced.
func (s *Service) attachResult(tx domain.Tx, res *ProposeResult) error {
	if res.Proposal.Status != domain.ProposalAccepted || res.Proposal.ResultContextID == "" {
		return nil
	}
	if res.Proposal.Kind == domain.KindCreateNamespace {
		ns, err := tx.GetNamespace(res.Proposal.ResultContextID)
		if err != nil {
			return err
		}
		res.Namespace = &ns
		return nil
	}
	c, err := tx.GetContext(res.Proposal.ResultContextID)
	if err != nil {
		return err
	}
	v, err := s.loadView(tx, c)
	if err != nil {
		return err
	}
	res.Context = &v
	return nil
}

// apply turns an accepted proposal into revisions (or a namespace) and marks
// it accepted. Everything happens in the caller's transaction.
func (s *Service) apply(tx domain.Tx, actor domain.Actor, p *domain.Proposal, trust domain.Trust, expires bool) error {
	now := s.now()
	authorKind, authorID := domain.ActorUser, actor.ID
	if p.AgentID != "" {
		// Provenance: the proposer wrote the content, even when the user approved it.
		authorKind, authorID = domain.ActorAgent, p.AgentID
	}

	switch p.Kind {
	case domain.KindCreate:
		c, err := s.createContext(tx, actor, p, p.Type, trust, expires, authorKind, authorID, now)
		if err != nil {
			return err
		}
		p.ResultContextID = c.ID

	case domain.KindRevise:
		c, err := tx.GetContext(p.TargetContextID)
		if err != nil {
			return err
		}
		if c.Status != domain.StatusActive {
			return domain.Invalidf("target context is %s", c.Status)
		}
		if c.CurrentVersion != p.BaseVersion {
			return domain.ErrStaleVersion
		}
		rev := domain.Revision{ID: newID("rev", now), ContextID: c.ID, Version: c.CurrentVersion + 1,
			Title: p.Title, Body: p.Body, Trust: trust, AuthorKind: authorKind, AuthorID: authorID,
			ProposalID: p.ID, CreatedAt: now}
		if err := tx.InsertRevision(rev); err != nil {
			return err
		}
		c.CurrentVersion, c.HeadTrust, c.Reviewed, c.UpdatedAt = rev.Version, trust, trust == domain.TrustUserVerified, now
		if c.Reviewed {
			c.ExpiresAt = nil
		}
		if err := tx.UpdateContext(c); err != nil {
			return err
		}
		if err := s.record(tx, actor, "context.revised", "context", c.ID, map[string]any{
			"version": rev.Version, "base_version": p.BaseVersion, "title": rev.Title, "body": rev.Body,
			"trust": rev.Trust, "proposal_id": p.ID,
		}); err != nil {
			return err
		}
		p.ResultContextID = c.ID

	case domain.KindSupersede:
		old, err := tx.GetContext(p.TargetContextID)
		if err != nil {
			return err
		}
		if old.Status != domain.StatusActive {
			return domain.Invalidf("target context is %s", old.Status)
		}
		if old.CurrentVersion != p.BaseVersion {
			return domain.ErrStaleVersion
		}
		c, err := s.createContext(tx, actor, p, p.Type, trust, false, authorKind, authorID, now)
		if err != nil {
			return err
		}
		old.Status, old.SupersededBy, old.UpdatedAt = domain.StatusSuperseded, c.ID, now
		if err := tx.UpdateContext(old); err != nil {
			return err
		}
		if err := tx.InsertRelationship(c.ID, old.ID, "supersedes", now); err != nil {
			return err
		}
		if err := s.record(tx, actor, "context.superseded", "context", old.ID, map[string]any{"superseded_by": c.ID, "proposal_id": p.ID}); err != nil {
			return err
		}
		p.ResultContextID = c.ID

	case domain.KindCreateNamespace:
		ns := domain.Namespace{ID: newID("ns", now), Name: p.Title, StorageMode: domain.StorageLocalOnly, CreatedAt: now}
		if err := tx.InsertNamespace(ns); err != nil {
			return err
		}
		if err := s.record(tx, actor, "namespace.created", "namespace", ns.ID, ns); err != nil {
			return err
		}
		if p.AgentID != "" {
			// The user's approval is the explicit grant for the agent that asked.
			if err := tx.InsertGrant(domain.Grant{AgentID: p.AgentID, NamespaceID: ns.ID}); err != nil {
				return err
			}
		}
		p.ResultContextID = ns.ID
	}

	p.Status = domain.ProposalAccepted
	p.DecidedAt, p.DecidedBy = &now, actor.ID
	if err := tx.UpdateProposal(*p); err != nil {
		return err
	}
	return s.record(tx, actor, "proposal.accepted", "proposal", p.ID, map[string]any{"result_id": p.ResultContextID, "trust": trust})
}

func (s *Service) createContext(tx domain.Tx, actor domain.Actor, p *domain.Proposal, typ domain.ContextType, trust domain.Trust,
	expires bool, authorKind domain.ActorKind, authorID string, now time.Time) (domain.Context, error) {
	c := domain.Context{ID: newID("ctx", now), NamespaceID: p.NamespaceID, Type: typ, Status: domain.StatusActive,
		CurrentVersion: 1, HeadTrust: trust, Reviewed: trust == domain.TrustUserVerified, CreatedAt: now, UpdatedAt: now}
	if expires {
		e := now.Add(ObservationTTL)
		c.ExpiresAt = &e
	}
	if err := tx.InsertContext(c); err != nil {
		return c, err
	}
	rev := domain.Revision{ID: newID("rev", now), ContextID: c.ID, Version: 1, Title: p.Title, Body: p.Body,
		Trust: trust, AuthorKind: authorKind, AuthorID: authorID, ProposalID: p.ID, CreatedAt: now}
	if err := tx.InsertRevision(rev); err != nil {
		return c, err
	}
	return c, s.record(tx, actor, "context.created", "context", c.ID, map[string]any{
		"namespace_id": c.NamespaceID, "type": c.Type, "version": 1, "title": rev.Title, "body": rev.Body,
		"trust": trust, "expires_at": c.ExpiresAt, "proposal_id": p.ID,
	})
}

// ApproveProposal accepts a pending proposal. Approval happens outside the
// chat, so the result is user_verified (ADR-013).
func (s *Service) ApproveProposal(ctx context.Context, actor domain.Actor, id string) (ProposeResult, error) {
	if err := policy.RequireUser(actor, "approving a proposal"); err != nil {
		return ProposeResult{}, err
	}
	var res ProposeResult
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		p, err := tx.GetProposal(id)
		if err != nil {
			return err
		}
		if p.Status != domain.ProposalPending {
			return domain.Invalidf("proposal is already %s", p.Status)
		}
		if err := s.apply(tx, actor, &p, domain.TrustUserVerified, false); err != nil {
			return err
		}
		res.Proposal = p
		return s.attachResult(tx, &res)
	})
	if err == nil {
		s.changed(ctx, res.Proposal.NamespaceID)
	}
	return res, err
}

// RejectProposal rejects a pending proposal.
func (s *Service) RejectProposal(ctx context.Context, actor domain.Actor, id, note string) error {
	if err := policy.RequireUser(actor, "rejecting a proposal"); err != nil {
		return err
	}
	nsID := ""
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		p, err := tx.GetProposal(id)
		if err != nil {
			return err
		}
		if p.Status != domain.ProposalPending {
			return domain.Invalidf("proposal is already %s", p.Status)
		}
		nsID = p.NamespaceID
		return s.decide(tx, actor, p, domain.ProposalRejected, note)
	})
	if err == nil {
		s.changed(ctx, nsID)
	}
	return err
}

func (s *Service) decide(tx domain.Tx, actor domain.Actor, p domain.Proposal, status domain.ProposalStatus, note string) error {
	now := s.now()
	p.Status, p.DecisionNote, p.DecidedAt, p.DecidedBy = status, note, &now, actor.ID
	if err := tx.UpdateProposal(p); err != nil {
		return err
	}
	return s.record(tx, actor, "proposal."+string(status), "proposal", p.ID, map[string]any{"note": note})
}

// Review is what is waiting for the user: proposals that need approval, and
// accepted context that isn't user_verified yet.
type Review struct {
	Pending    []domain.Proposal
	Unreviewed []ContextView
}

// ReviewQueue returns the review queue (ADR-013).
func (s *Service) ReviewQueue(ctx context.Context, actor domain.Actor) (Review, error) {
	if err := policy.RequireUser(actor, "reviewing"); err != nil {
		return Review{}, err
	}
	var r Review
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		r.Pending, err = tx.ListProposals(domain.ProposalFilter{Statuses: []domain.ProposalStatus{domain.ProposalPending}})
		if err != nil {
			return err
		}
		now := s.now()
		cs, err := tx.ListContexts(domain.ContextFilter{Statuses: []domain.ContextStatus{domain.StatusActive}, Unreviewed: true, NotExpiredAt: &now})
		if err != nil {
			return err
		}
		for _, c := range cs {
			v, err := s.loadView(tx, c)
			if err != nil {
				return err
			}
			r.Unreviewed = append(r.Unreviewed, v)
		}
		return nil
	})
	return r, err
}

// GetProposal returns one proposal (user only).
func (s *Service) GetProposal(ctx context.Context, actor domain.Actor, id string) (domain.Proposal, error) {
	if err := policy.RequireUser(actor, "reading proposals"); err != nil {
		return domain.Proposal{}, err
	}
	var p domain.Proposal
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		p, err = tx.GetProposal(id)
		return err
	})
	return p, err
}
