package application

import (
	"context"

	"centrext/internal/domain"
	"centrext/internal/policy"
	"centrext/internal/workspace"
)

// ExportTypes are the types `centrext export` writes (ADR-020, ADR-022).
var ExportTypes = []domain.ContextType{domain.TypeDecision, domain.TypeArchitecture, domain.TypeConvention}

// SetStorageMode changes a namespace's storage policy (ADR-009). Only the
// user can, because it decides whether content may leave the device.
func (s *Service) SetStorageMode(ctx context.Context, actor domain.Actor, namespaceID string, mode domain.StorageMode) (domain.Namespace, error) {
	if err := policy.RequireUser(actor, "changing storage policy"); err != nil {
		return domain.Namespace{}, err
	}
	if !mode.Valid() {
		return domain.Namespace{}, domain.Invalidf("unknown storage mode %q", mode)
	}
	var ns domain.Namespace
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		var err error
		if ns, err = tx.GetNamespace(namespaceID); err != nil {
			return err
		}
		if ns.StorageMode == mode {
			return nil
		}
		from := ns.StorageMode
		if err := tx.SetNamespaceStorage(ns.ID, mode); err != nil {
			return err
		}
		ns.StorageMode = mode
		return s.record(tx, actor, "namespace.storage_changed", "namespace", ns.ID, map[string]any{"from": from, "to": mode})
	})
	return ns, err
}

// LookupNamespace finds the namespace for a workspace identity without
// creating one (unlike ResolveNamespace).
func (s *Service) LookupNamespace(ctx context.Context, actor domain.Actor, id workspace.Identity) (domain.Namespace, error) {
	var ns domain.Namespace
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		switch id.Kind {
		case workspace.KindMarker:
			ns, err = tx.GetNamespace(id.Value)
		case workspace.KindGitRemote, workspace.KindRepoRoot:
			ns, err = tx.GetNamespaceByAlias(id.Value)
		default:
			ns, err = tx.GetNamespaceByName("", PersonalNamespace)
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

// ExportSnapshot returns what `centrext export` may write for a namespace:
// active, user_verified decisions, architecture and conventions. Only the
// user can export, and never from a local_only namespace, because exported
// files get committed and pushed (ADR-020). Each export is audited.
func (s *Service) ExportSnapshot(ctx context.Context, actor domain.Actor, namespaceID string) (domain.Namespace, []ContextView, error) {
	if err := policy.RequireUser(actor, "exporting context"); err != nil {
		return domain.Namespace{}, nil, err
	}
	var ns domain.Namespace
	var out []ContextView
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		var err error
		if ns, err = tx.GetNamespace(namespaceID); err != nil {
			return err
		}
		if ns.StorageMode == domain.StorageLocalOnly {
			return domain.Forbiddenf("namespace %q is local_only, so its content may not leave this device. To allow exports, change its policy with `centrext namespace policy %s local_primary`", ns.Name, ns.Name)
		}
		now := s.now()
		cs, err := tx.ListContexts(domain.ContextFilter{NamespaceIDs: []string{ns.ID}, Types: ExportTypes,
			Statuses: []domain.ContextStatus{domain.StatusActive}, NotExpiredAt: &now})
		if err != nil {
			return err
		}
		ids := []string{}
		for _, c := range cs {
			if c.HeadTrust != domain.TrustUserVerified {
				continue
			}
			v, err := s.loadView(tx, c)
			if err != nil {
				return err
			}
			out = append(out, v)
			ids = append(ids, c.ID)
		}
		return s.audit(tx, actor, "namespace.exported", "namespace:"+ns.ID, map[string]any{"contexts": ids})
	})
	return ns, out, err
}
