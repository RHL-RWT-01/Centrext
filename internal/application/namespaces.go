package application

import (
	"context"
	"strings"

	"centrext/internal/domain"
	"centrext/internal/policy"
)

// NewNamespace is the input to CreateNamespace.
type NewNamespace struct {
	Name        string
	ParentID    string
	StorageMode domain.StorageMode // defaults to local_only
}

// CreateNamespace creates a namespace. Only the user can do this directly;
// agents propose one with KindCreateNamespace.
func (s *Service) CreateNamespace(ctx context.Context, actor domain.Actor, in NewNamespace) (domain.Namespace, error) {
	if err := policy.RequireUser(actor, "creating a namespace"); err != nil {
		return domain.Namespace{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := domain.ValidateName(in.Name); err != nil {
		return domain.Namespace{}, err
	}
	if in.StorageMode == "" {
		in.StorageMode = domain.StorageLocalOnly
	}
	if !in.StorageMode.Valid() {
		return domain.Namespace{}, domain.Invalidf("unknown storage mode %q", in.StorageMode)
	}
	var ns domain.Namespace
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		if in.ParentID != "" {
			if _, err := tx.GetNamespace(in.ParentID); err != nil {
				return err
			}
		}
		now := s.now()
		ns = domain.Namespace{ID: newID("ns", now), ParentID: in.ParentID, Name: in.Name, StorageMode: in.StorageMode, CreatedAt: now}
		if err := tx.InsertNamespace(ns); err != nil {
			return err
		}
		return s.record(tx, actor, "namespace.created", "namespace", ns.ID, ns)
	})
	return ns, err
}

// ListNamespaces lists namespaces the actor can access.
func (s *Service) ListNamespaces(ctx context.Context, actor domain.Actor) ([]domain.Namespace, error) {
	var out []domain.Namespace
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		all, err := tx.ListNamespaces()
		if err != nil {
			return err
		}
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		for _, n := range all {
			if policy.CanAccessNamespace(actor, n.ID, grants) == nil {
				out = append(out, n)
			}
		}
		return nil
	})
	return out, err
}

// FindNamespace looks a namespace up by id, or by name among root namespaces.
func (s *Service) FindNamespace(ctx context.Context, actor domain.Actor, idOrName string) (domain.Namespace, error) {
	var ns domain.Namespace
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		ns, err = tx.GetNamespace(idOrName)
		if err == domain.ErrNotFound {
			ns, err = tx.GetNamespaceByName("", idOrName)
		}
		if err != nil {
			return err
		}
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		return policy.CanAccessNamespace(actor, ns.ID, grants)
	})
	return ns, err
}

// AddNamespaceAlias links an identity (e.g. a normalized git remote) to a
// namespace. Linking is always explicit (ADR-017).
func (s *Service) AddNamespaceAlias(ctx context.Context, actor domain.Actor, namespaceID, identity string) error {
	if err := policy.RequireUser(actor, "linking a namespace"); err != nil {
		return err
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return domain.Invalidf("identity is required")
	}
	return s.store.InTx(ctx, func(tx domain.Tx) error {
		if _, err := tx.GetNamespace(namespaceID); err != nil {
			return err
		}
		if err := tx.AddNamespaceAlias(namespaceID, identity); err != nil {
			return err
		}
		return s.record(tx, actor, "namespace.alias_added", "namespace", namespaceID, map[string]string{"identity": identity})
	})
}

func grantsFor(tx domain.Tx, actor domain.Actor) ([]domain.Grant, error) {
	if actor.IsUser() || actor.Agent == nil {
		return nil, nil
	}
	return tx.ListGrants(actor.Agent.ID)
}
