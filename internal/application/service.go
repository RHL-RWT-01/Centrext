// Package application holds Centrext's use cases. CLI, MCP and REST all call
// these services; none of them make policy decisions themselves.
package application

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"centrext/internal/domain"
)

// ObservationTTL is how long a silent observation lives unless approved (ADR-015).
const ObservationTTL = 30 * 24 * time.Hour

// Service is the application layer.
type Service struct {
	store    domain.Store
	now      func() time.Time
	onChange func(ctx context.Context, namespaceID string)
}

// Option configures a Service.
type Option func(*Service)

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithOnChange registers a callback that runs after a committed change to a
// namespace's context or review queue. Delivery routes that keep copies (the
// Cursor rules file, ADR-014) use it to stay current. It runs outside the
// transaction; its failures never undo or fail the change.
func WithOnChange(f func(ctx context.Context, namespaceID string)) Option {
	return func(s *Service) { s.onChange = f }
}

func (s *Service) changed(ctx context.Context, namespaceID string) {
	if s.onChange != nil && namespaceID != "" {
		s.onChange(ctx, namespaceID)
	}
}

// New creates a Service.
func New(store domain.Store, opts ...Option) *Service {
	s := &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
	for _, o := range opts {
		o(s)
	}
	return s
}

// newID returns a time-ordered unique id: prefix_<12 hex ms><16 hex random>.
func newID(prefix string, now time.Time) string {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return fmt.Sprintf("%s_%012x%s", prefix, now.UnixMilli(), hex.EncodeToString(r[:]))
}

// record writes the audit event and the outbox entry for one change, inside
// the caller's transaction (invariant 4).
func (s *Service) record(tx domain.Tx, actor domain.Actor, action, entity, entityID string, payload any) error {
	now := s.now()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if err := tx.AppendAudit(domain.AuditEvent{
		ID: newID("aud", now), At: now, ActorKind: actor.Kind, ActorID: actor.ID,
		Action: action, Subject: entity + ":" + entityID, Detail: string(body),
	}); err != nil {
		return err
	}
	opID := newID("op", now)
	return tx.AppendOutbox(domain.OutboxEntry{
		ID: opID, IdempotencyKey: opID, Op: action, Entity: entity, EntityID: entityID,
		Payload: string(body), CreatedAt: now,
	})
}

// audit writes an audit event without an outbox entry, for events that change
// no replicated state (e.g. a denied proposal).
func (s *Service) audit(tx domain.Tx, actor domain.Actor, action, subject string, detail any) error {
	now := s.now()
	body, _ := json.Marshal(detail)
	return tx.AppendAudit(domain.AuditEvent{
		ID: newID("aud", now), At: now, ActorKind: actor.Kind, ActorID: actor.ID,
		Action: action, Subject: subject, Detail: string(body),
	})
}

// PersonalNamespace is the name of the namespace created at init.
const PersonalNamespace = "Personal"

// Init creates the local user and the personal namespace. It is idempotent.
func (s *Service) Init(ctx context.Context, userName string) (domain.User, error) {
	if err := domain.ValidateName(userName); err != nil {
		return domain.User{}, err
	}
	var user domain.User
	err := s.store.InTx(ctx, func(tx domain.Tx) error {
		u, err := tx.FirstUser()
		if err == nil {
			user = u
			return nil
		}
		if err != domain.ErrNotFound {
			return err
		}
		now := s.now()
		user = domain.User{ID: newID("usr", now), Name: userName, CreatedAt: now}
		if err := tx.InsertUser(user); err != nil {
			return err
		}
		actor := domain.Actor{Kind: domain.ActorUser, ID: user.ID}
		if err := s.record(tx, actor, "user.created", "user", user.ID, map[string]any{"name": user.Name}); err != nil {
			return err
		}
		ns := domain.Namespace{ID: newID("ns", now), Name: PersonalNamespace, StorageMode: domain.StorageLocalOnly, CreatedAt: now}
		if err := tx.InsertNamespace(ns); err != nil {
			return err
		}
		return s.record(tx, actor, "namespace.created", "namespace", ns.ID, ns)
	})
	return user, err
}

// LocalUser returns the actor for the local owner, used by the CLI.
func (s *Service) LocalUser(ctx context.Context) (domain.Actor, error) {
	var actor domain.Actor
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		u, err := tx.FirstUser()
		if err != nil {
			if err == domain.ErrNotFound {
				return fmt.Errorf("centrext is not initialized; run `centrext init`")
			}
			return err
		}
		actor = domain.Actor{Kind: domain.ActorUser, ID: u.ID}
		return nil
	})
	return actor, err
}
