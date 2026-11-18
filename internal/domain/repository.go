package domain

import (
	"context"
	"time"
)

// Store runs work in transactions. Everything a use case writes (revision,
// proposal, audit event, outbox entry) goes through one Tx, so it commits or
// fails together.
type Store interface {
	// InTx runs fn in a write transaction.
	InTx(ctx context.Context, fn func(Tx) error) error
	// Read runs fn in a read-only transaction.
	Read(ctx context.Context, fn func(Tx) error) error
}

// Tx is the set of repositories available inside a transaction.
type Tx interface {
	UserRepo
	NamespaceRepo
	ContextRepo
	ProposalRepo
	AgentRepo
	AuditRepo
	OutboxRepo
	UsageRepo
}

type UserRepo interface {
	InsertUser(User) error
	GetUser(id string) (User, error)
	FirstUser() (User, error)
}

type NamespaceRepo interface {
	InsertNamespace(Namespace) error
	GetNamespace(id string) (Namespace, error)
	GetNamespaceByName(parentID, name string) (Namespace, error)
	ListNamespaces() ([]Namespace, error)
	AddNamespaceAlias(namespaceID, identity string) error
	GetNamespaceByAlias(identity string) (Namespace, error)
}

// ContextFilter selects contexts. Zero values mean "any".
type ContextFilter struct {
	NamespaceIDs []string
	Types        []ContextType
	Statuses     []ContextStatus
	Unreviewed   bool
	NotExpiredAt *time.Time
}

// SearchFilter selects full-text matches. Only active contexts are searched.
type SearchFilter struct {
	Match        string // FTS5 match expression, built by the caller from user text
	NamespaceIDs []string
	Types        []ContextType
	NotExpiredAt time.Time
	Limit        int
}

// SearchHit is one match. Lexical is higher for better matches.
type SearchHit struct {
	Context Context
	Lexical float64
}

type ContextRepo interface {
	SearchContexts(SearchFilter) ([]SearchHit, error)
	InsertContext(Context) error
	UpdateContext(Context) error
	GetContext(id string) (Context, error)
	ListContexts(ContextFilter) ([]Context, error)
	InsertRevision(Revision) error
	GetRevision(contextID string, version int) (Revision, error)
	ListRevisions(contextID string) ([]Revision, error)
	InsertRelationship(fromID, toID, kind string, at time.Time) error
}

// ProposalFilter selects proposals. Zero values mean "any".
type ProposalFilter struct {
	Statuses []ProposalStatus
	AgentID  string
}

type ProposalRepo interface {
	InsertProposal(Proposal) error
	UpdateProposal(Proposal) error
	GetProposal(id string) (Proposal, error)
	GetProposalByIdempotencyKey(agentID, key string) (Proposal, error)
	ListProposals(ProposalFilter) ([]Proposal, error)
	CountPendingProposals(namespaceID string) (int, error)
}

type AgentRepo interface {
	InsertAgent(Agent) error
	UpdateAgent(Agent) error
	GetAgent(id string) (Agent, error)
	GetAgentByTokenHash(hash string) (Agent, error)
	ListAgents() ([]Agent, error)
	InsertGrant(Grant) error
	ListGrants(agentID string) ([]Grant, error)
}

type AuditRepo interface {
	AppendAudit(AuditEvent) error
	ListAudit(limit int) ([]AuditEvent, error)
}

type OutboxRepo interface {
	AppendOutbox(OutboxEntry) error
	CountOutbox() (int, error)
}

type UsageRepo interface {
	RecordUsage(UsageEvent) error
}
