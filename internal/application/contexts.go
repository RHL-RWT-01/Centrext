package application

import (
	"context"

	"centrext/internal/domain"
	"centrext/internal/policy"
)

// ContextView is a context with its head revision.
type ContextView struct {
	Context domain.Context
	Head    domain.Revision
}

// ListQuery selects contexts to list.
type ListQuery struct {
	NamespaceID     string // empty: every namespace the actor can access
	Types           []domain.ContextType
	IncludeInactive bool // superseded, deleted and expired; user only
}

// visibleToAgent hides tombstones and expired items from agents.
func (s *Service) visibleToAgent(c domain.Context) bool {
	return c.Status != domain.StatusDeleted && !c.Expired(s.now())
}

func (s *Service) loadView(tx domain.Tx, c domain.Context) (ContextView, error) {
	head, err := tx.GetRevision(c.ID, c.CurrentVersion)
	return ContextView{Context: c, Head: head}, err
}

// authorizeContext loads a context and checks the actor may read it.
func (s *Service) authorizeContext(tx domain.Tx, actor domain.Actor, id string) (domain.Context, error) {
	c, err := tx.GetContext(id)
	if err != nil {
		return c, err
	}
	grants, err := grantsFor(tx, actor)
	if err != nil {
		return c, err
	}
	if err := policy.CanAccessNamespace(actor, c.NamespaceID, grants); err != nil {
		// Don't reveal that the context exists.
		return c, domain.ErrNotFound
	}
	if !actor.IsUser() && !s.visibleToAgent(c) {
		return c, domain.ErrNotFound
	}
	return c, nil
}

// GetContext returns a context and its head revision. Agent reads are
// recorded as usage events (ADR-015).
func (s *Service) GetContext(ctx context.Context, actor domain.Actor, id string) (ContextView, error) {
	var view ContextView
	fn := func(tx domain.Tx) error {
		c, err := s.authorizeContext(tx, actor, id)
		if err != nil {
			return err
		}
		view, err = s.loadView(tx, c)
		if err != nil {
			return err
		}
		if actor.IsUser() {
			return nil
		}
		now := s.now()
		return tx.RecordUsage(domain.UsageEvent{ID: newID("use", now), At: now, AgentID: actor.ID,
			ContextID: c.ID, Version: c.CurrentVersion, Kind: "retrieved"})
	}
	if actor.IsUser() {
		return view, s.store.Read(ctx, fn)
	}
	return view, s.store.InTx(ctx, fn)
}

// ListContexts lists contexts the actor can access.
func (s *Service) ListContexts(ctx context.Context, actor domain.Actor, q ListQuery) ([]ContextView, error) {
	if q.IncludeInactive {
		if err := policy.RequireUser(actor, "listing inactive context"); err != nil {
			return nil, err
		}
	}
	var out []ContextView
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		var nsIDs []string
		if q.NamespaceID != "" {
			if err := policy.CanAccessNamespace(actor, q.NamespaceID, grants); err != nil {
				return err
			}
			nsIDs = []string{q.NamespaceID}
		} else if !actor.IsUser() {
			all, err := tx.ListNamespaces()
			if err != nil {
				return err
			}
			for _, n := range all {
				if policy.CanAccessNamespace(actor, n.ID, grants) == nil {
					nsIDs = append(nsIDs, n.ID)
				}
			}
			if len(nsIDs) == 0 {
				return nil
			}
		}
		f := domain.ContextFilter{NamespaceIDs: nsIDs, Types: q.Types}
		if !q.IncludeInactive {
			now := s.now()
			f.Statuses = []domain.ContextStatus{domain.StatusActive}
			f.NotExpiredAt = &now
		}
		cs, err := tx.ListContexts(f)
		if err != nil {
			return err
		}
		for _, c := range cs {
			v, err := s.loadView(tx, c)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// History returns every revision of a context. Old revisions need the same
// authorization as the current one.
func (s *Service) History(ctx context.Context, actor domain.Actor, id string) ([]domain.Revision, error) {
	var out []domain.Revision
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		if _, err := s.authorizeContext(tx, actor, id); err != nil {
			return err
		}
		var err error
		out, err = tx.ListRevisions(id)
		return err
	})
	return out, err
}

// VerifyContext marks a context's current content as user_verified by adding
// a new revision with the same content (revisions are never edited). Only
// the user can do this, outside the chat (ADR-013).
func (s *Service) VerifyContext(ctx context.Context, actor domain.Actor, id string, baseVersion int) (ContextView, error) {
	if err := policy.RequireUser(actor, "verifying context"); err != nil {
		return ContextView{}, err
	}
	var view ContextView
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		c, err := tx.GetContext(id)
		if err != nil {
			return err
		}
		if c.Status != domain.StatusActive {
			return domain.Invalidf("context is %s", c.Status)
		}
		if c.CurrentVersion != baseVersion {
			return domain.ErrStaleVersion
		}
		if c.Reviewed {
			view, err = s.loadView(tx, c)
			return err
		}
		head, err := tx.GetRevision(c.ID, c.CurrentVersion)
		if err != nil {
			return err
		}
		now := s.now()
		rev := domain.Revision{ID: newID("rev", now), ContextID: c.ID, Version: c.CurrentVersion + 1,
			Title: head.Title, Body: head.Body, Trust: domain.TrustUserVerified,
			AuthorKind: domain.ActorUser, AuthorID: actor.ID, CreatedAt: now}
		if err := tx.InsertRevision(rev); err != nil {
			return err
		}
		c.CurrentVersion, c.HeadTrust, c.Reviewed, c.ExpiresAt, c.UpdatedAt = rev.Version, rev.Trust, true, nil, now
		if err := tx.UpdateContext(c); err != nil {
			return err
		}
		if err := s.record(tx, actor, "context.verified", "context", c.ID, map[string]any{"version": rev.Version, "from_version": baseVersion}); err != nil {
			return err
		}
		view = ContextView{Context: c, Head: rev}
		return nil
	})
	if err == nil {
		s.changed(ctx, view.Context.NamespaceID)
	}
	return view, err
}

// DeleteContext tombstones a context (invariant 8). Agents can never delete.
func (s *Service) DeleteContext(ctx context.Context, actor domain.Actor, id string, baseVersion int) error {
	if err := policy.RequireUser(actor, "deleting context"); err != nil {
		return err
	}
	nsID := ""
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		c, err := tx.GetContext(id)
		if err != nil {
			return err
		}
		if c.Status == domain.StatusDeleted {
			return nil
		}
		nsID = c.NamespaceID
		if c.CurrentVersion != baseVersion {
			return domain.ErrStaleVersion
		}
		c.Status, c.UpdatedAt = domain.StatusDeleted, s.now()
		if err := tx.UpdateContext(c); err != nil {
			return err
		}
		return s.record(tx, actor, "context.deleted", "context", c.ID, map[string]any{"version": c.CurrentVersion})
	})
	if err == nil {
		s.changed(ctx, nsID)
	}
	return err
}
