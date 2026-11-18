// Package domain holds Centrext's entities, enums, validation and repository
// interfaces. It has no I/O and imports nothing from transport or infrastructure.
package domain

import "time"

// ContextType is what kind of knowledge a context holds. It is independent of trust.
type ContextType string

const (
	TypeFact         ContextType = "fact"
	TypePreference   ContextType = "preference"
	TypeDecision     ContextType = "decision"
	TypeArchitecture ContextType = "architecture"
	TypeObservation  ContextType = "observation"
	TypeConvention   ContextType = "convention"
	TypeTask         ContextType = "task"
	TypeEntity       ContextType = "entity"
)

func (t ContextType) Valid() bool {
	switch t {
	case TypeFact, TypePreference, TypeDecision, TypeArchitecture,
		TypeObservation, TypeConvention, TypeTask, TypeEntity:
		return true
	}
	return false
}

// Trust is assigned by Centrext from how an item arrived (ADR-013), never
// taken from an agent's claim.
type Trust string

const (
	TrustUserVerified     Trust = "user_verified"
	TrustTrustedSource    Trust = "trusted_source"
	TrustSessionConfirmed Trust = "session_confirmed"
	TrustAgentReported    Trust = "agent_reported"
	TrustUntrusted        Trust = "untrusted"
)

// Rank orders trust levels; higher is more trusted.
func (t Trust) Rank() int {
	switch t {
	case TrustUserVerified:
		return 5
	case TrustTrustedSource:
		return 4
	case TrustSessionConfirmed:
		return 3
	case TrustAgentReported:
		return 2
	case TrustUntrusted:
		return 1
	}
	return 0
}

func (t Trust) Valid() bool { return t.Rank() > 0 }

// StorageMode is a namespace's storage policy (ADR-009).
type StorageMode string

const (
	StorageLocalOnly    StorageMode = "local_only"
	StorageLocalPrimary StorageMode = "local_primary"
	StorageCloudPrimary StorageMode = "cloud_primary"
	StorageShared       StorageMode = "shared"
)

func (m StorageMode) Valid() bool {
	switch m {
	case StorageLocalOnly, StorageLocalPrimary, StorageCloudPrimary, StorageShared:
		return true
	}
	return false
}

// ContextStatus is the lifecycle state of a context.
type ContextStatus string

const (
	StatusActive     ContextStatus = "active"
	StatusSuperseded ContextStatus = "superseded"
	StatusDeleted    ContextStatus = "deleted" // tombstone (invariant 8)
)

// ProposalKind is what a proposal asks for (ADR-007).
type ProposalKind string

const (
	KindCreate          ProposalKind = "create"
	KindRevise          ProposalKind = "revise"
	KindSupersede       ProposalKind = "supersede"
	KindCreateNamespace ProposalKind = "create_namespace"
)

func (k ProposalKind) Valid() bool {
	switch k {
	case KindCreate, KindRevise, KindSupersede, KindCreateNamespace:
		return true
	}
	return false
}

// ProposalOrigin records how a proposal arrived (ADR-015).
type ProposalOrigin string

const (
	OriginExplicit  ProposalOrigin = "explicit"  // user said "remember this"
	OriginSuggested ProposalOrigin = "suggested" // agent asked, user agreed
	OriginSilent    ProposalOrigin = "silent"    // agent proposed without asking
	OriginUser      ProposalOrigin = "user"      // written by the user outside the chat (CLI)
)

func (o ProposalOrigin) Valid() bool {
	switch o {
	case OriginExplicit, OriginSuggested, OriginSilent, OriginUser:
		return true
	}
	return false
}

// ProposalStatus is where a proposal is in review.
type ProposalStatus string

const (
	ProposalPending  ProposalStatus = "pending"
	ProposalAccepted ProposalStatus = "accepted"
	ProposalRejected ProposalStatus = "rejected"
)

// ActorKind distinguishes the human owner from an agent.
type ActorKind string

const (
	ActorUser  ActorKind = "user"
	ActorAgent ActorKind = "agent"
)

// Actor is who is calling. User actors only exist on channels outside the
// chat (CLI, later dashboard). Agent actors come from an authenticated token.
type Actor struct {
	Kind  ActorKind
	ID    string
	Agent *Agent // set when Kind == ActorAgent
}

func (a Actor) IsUser() bool { return a.Kind == ActorUser }

// User is the human identity. Phase 1 has one local user.
type User struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// Namespace scopes ownership. Nesting never grants access by itself.
type Namespace struct {
	ID          string
	ParentID    string // empty for a root namespace
	Name        string
	StorageMode StorageMode
	CreatedAt   time.Time
}

// Context is the stable identity of one piece of knowledge (ADR-006).
type Context struct {
	ID             string
	NamespaceID    string
	Type           ContextType
	Status         ContextStatus
	CurrentVersion int
	HeadTrust      Trust      // trust of the head revision
	Reviewed       bool       // head revision is user_verified
	ExpiresAt      *time.Time // silent observations expire unless approved (ADR-015)
	SupersededBy   string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Expired reports whether the context has passed its expiry.
func (c Context) Expired(now time.Time) bool {
	return c.ExpiresAt != nil && !now.Before(*c.ExpiresAt)
}

// Revision is an immutable version of a context. Never overwritten (invariant 3).
type Revision struct {
	ID         string
	ContextID  string
	Version    int
	Title      string
	Body       string
	Trust      Trust
	AuthorKind ActorKind
	AuthorID   string
	ProposalID string
	CreatedAt  time.Time
}

// Proposal is candidate knowledge awaiting acceptance (ADR-007).
type Proposal struct {
	ID              string
	NamespaceID     string
	Kind            ProposalKind
	Origin          ProposalOrigin
	Status          ProposalStatus
	TargetContextID string // revise / supersede
	BaseVersion     int    // revise / supersede
	Type            ContextType
	Title           string
	Body            string
	Trust           Trust // assigned by Centrext
	AgentID         string
	IdempotencyKey  string
	ResultContextID string // context created or changed when accepted
	DecisionNote    string
	CreatedAt       time.Time
	DecidedAt       *time.Time
	DecidedBy       string
}

// Agent is one AI client install, authenticated by a token (ADR-018).
type Agent struct {
	ID         string
	Name       string
	Client     string
	TokenHash  string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	RevokedAt  *time.Time
}

func (a Agent) Active() bool { return a.RevokedAt == nil }

// Grant gives an agent access to one namespace, or to all when NamespaceID is empty.
type Grant struct {
	AgentID     string
	NamespaceID string
}

// AuditEvent is append-only security history.
type AuditEvent struct {
	ID        string
	At        time.Time
	ActorKind ActorKind
	ActorID   string
	Action    string
	Subject   string
	Detail    string
}

// OutboxEntry is a durable record of a local change, written in the same
// transaction as the change (invariant 4), for sync in 0.4.
type OutboxEntry struct {
	ID             string
	IdempotencyKey string
	Op             string
	Entity         string
	EntityID       string
	Payload        string
	CreatedAt      time.Time
}

// UsageEvent records retrieval and use, for capture metrics (ADR-015).
// It is separate from the security audit log.
type UsageEvent struct {
	ID        string
	At        time.Time
	AgentID   string
	ContextID string
	Version   int
	Kind      string // "retrieved" | "used"
}
