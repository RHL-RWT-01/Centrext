package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"centrext/internal/domain"
	"centrext/internal/policy"
	"centrext/internal/workspace"
)

// Resolved is the namespace a session works in, and how it was found.
type Resolved struct {
	Namespace domain.Namespace
	How       workspace.Kind
	Identity  string
	Created   bool   // auto-created for a repo seen for the first time
	Note      string // e.g. ambiguous remotes
}

// ResolveNamespace maps a workspace identity to a namespace (ADR-017). Repos
// seen for the first time get a local_only namespace with the identity as its
// alias. Resolving never grants access: the actor must already have it.
func (s *Service) ResolveNamespace(ctx context.Context, actor domain.Actor, id workspace.Identity) (Resolved, error) {
	r := Resolved{How: id.Kind, Identity: id.Value}
	if id.Ambiguous {
		r.Note = fmt.Sprintf("several git remotes (%v) and none is origin; run `centrext link NAMESPACE` in the repo to pin one", id.RemoteNames)
	}
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		var err error
		switch id.Kind {
		case workspace.KindMarker:
			r.Namespace, err = tx.GetNamespace(id.Value)
			if err == domain.ErrNotFound {
				return domain.Invalidf("%s in %s names namespace %s, which doesn't exist here", workspace.MarkerFile, id.Dir, id.Value)
			}
		case workspace.KindGitRemote, workspace.KindRepoRoot:
			r.Namespace, err = tx.GetNamespaceByAlias(id.Value)
			if err == domain.ErrNotFound {
				r.Namespace, err = s.autoCreate(tx, actor, id)
				r.Created = err == nil
			}
		default:
			r.Namespace, err = tx.GetNamespaceByName("", PersonalNamespace)
		}
		if err != nil {
			return err
		}
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		if err := policy.CanAccessNamespace(actor, r.Namespace.ID, grants); err != nil {
			return domain.Forbiddenf("this agent has no access to namespace %q (%s); grant it with `centrext agent grant %s %s`",
				r.Namespace.Name, r.Namespace.ID, actor.ID, r.Namespace.ID)
		}
		return nil
	})
	return r, err
}

// autoCreate makes a local_only namespace for a repo (ADR-017).
func (s *Service) autoCreate(tx domain.Tx, actor domain.Actor, id workspace.Identity) (domain.Namespace, error) {
	now := s.now()
	name := id.Name
	if name == "" {
		name = "project"
	}
	ns := domain.Namespace{ID: newID("ns", now), Name: name, StorageMode: domain.StorageLocalOnly, CreatedAt: now}
	err := tx.InsertNamespace(ns)
	if err == domain.ErrConflict {
		// Another root namespace has this name; keep it distinct and stable.
		sum := sha256.Sum256([]byte(id.Value))
		ns.Name = name + "-" + hex.EncodeToString(sum[:])[:6]
		err = tx.InsertNamespace(ns)
	}
	if err != nil {
		return ns, err
	}
	if err := tx.AddNamespaceAlias(ns.ID, id.Value); err != nil {
		return ns, err
	}
	return ns, s.record(tx, actor, "namespace.auto_created", "namespace", ns.ID, map[string]any{
		"name": ns.Name, "identity": id.Value, "storage_mode": ns.StorageMode,
	})
}

// GrantAgent gives an agent access to a namespace ("" = all namespaces).
func (s *Service) GrantAgent(ctx context.Context, actor domain.Actor, agentID, namespaceID string) error {
	if err := policy.RequireUser(actor, "granting access"); err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx domain.Tx) error {
		if _, err := tx.GetAgent(agentID); err != nil {
			return err
		}
		if namespaceID != "" {
			if _, err := tx.GetNamespace(namespaceID); err != nil {
				return err
			}
		}
		if err := tx.InsertGrant(domain.Grant{AgentID: agentID, NamespaceID: namespaceID}); err != nil {
			return err
		}
		return s.record(tx, actor, "agent.granted", "agent", agentID, map[string]string{"namespace_id": namespaceID})
	})
}
