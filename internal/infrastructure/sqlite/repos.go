package sqlite

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"centrext/internal/domain"
)

// txn implements domain.Tx on one *sql.Tx.
type txn struct{ tx *sql.Tx }

var _ domain.Tx = (*txn)(nil)

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(v int64) time.Time { return time.UnixMilli(v).UTC() }

func msPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func fromNullMs(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMs(v.Int64)
	return &t
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- users

func (t *txn) InsertUser(u domain.User) error {
	_, err := t.tx.Exec(`insert into users(id, name, created_at) values(?,?,?)`, u.ID, u.Name, ms(u.CreatedAt))
	return err
}

func (t *txn) GetUser(id string) (domain.User, error) {
	var u domain.User
	var c int64
	err := t.tx.QueryRow(`select id, name, created_at from users where id=?`, id).Scan(&u.ID, &u.Name, &c)
	u.CreatedAt = fromMs(c)
	return u, notFound(err)
}

func (t *txn) FirstUser() (domain.User, error) {
	var u domain.User
	var c int64
	err := t.tx.QueryRow(`select id, name, created_at from users order by created_at limit 1`).Scan(&u.ID, &u.Name, &c)
	u.CreatedAt = fromMs(c)
	return u, notFound(err)
}

// ---- namespaces

const nsCols = `id, parent_id, name, storage_mode, created_at`

func scanNamespace(r interface{ Scan(...any) error }) (domain.Namespace, error) {
	var n domain.Namespace
	var c int64
	err := r.Scan(&n.ID, &n.ParentID, &n.Name, &n.StorageMode, &c)
	n.CreatedAt = fromMs(c)
	return n, err
}

func (t *txn) InsertNamespace(n domain.Namespace) error {
	_, err := t.tx.Exec(`insert into namespaces(`+nsCols+`) values(?,?,?,?,?)`,
		n.ID, n.ParentID, n.Name, n.StorageMode, ms(n.CreatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return domain.ErrConflict
	}
	return err
}

func (t *txn) GetNamespace(id string) (domain.Namespace, error) {
	n, err := scanNamespace(t.tx.QueryRow(`select `+nsCols+` from namespaces where id=?`, id))
	return n, notFound(err)
}

func (t *txn) GetNamespaceByName(parentID, name string) (domain.Namespace, error) {
	n, err := scanNamespace(t.tx.QueryRow(`select `+nsCols+` from namespaces where parent_id=? and name=?`, parentID, name))
	return n, notFound(err)
}

func (t *txn) ListNamespaces() ([]domain.Namespace, error) {
	rows, err := t.tx.Query(`select ` + nsCols + ` from namespaces order by name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Namespace
	for rows.Next() {
		n, err := scanNamespace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (t *txn) AddNamespaceAlias(namespaceID, identity string) error {
	_, err := t.tx.Exec(`insert into namespace_aliases(identity, namespace_id) values(?,?)`, identity, namespaceID)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return domain.ErrConflict
	}
	return err
}

func (t *txn) GetNamespaceByAlias(identity string) (domain.Namespace, error) {
	n, err := scanNamespace(t.tx.QueryRow(`select n.id, n.parent_id, n.name, n.storage_mode, n.created_at
		from namespaces n join namespace_aliases a on a.namespace_id = n.id where a.identity=?`, identity))
	return n, notFound(err)
}

// ---- contexts and revisions

const ctxCols = `id, namespace_id, type, status, current_version, head_trust, reviewed, expires_at, superseded_by, created_at, updated_at`

func scanContext(r interface{ Scan(...any) error }) (domain.Context, error) {
	var c domain.Context
	var reviewed int
	var exp sql.NullInt64
	var created, updated int64
	err := r.Scan(&c.ID, &c.NamespaceID, &c.Type, &c.Status, &c.CurrentVersion, &c.HeadTrust,
		&reviewed, &exp, &c.SupersededBy, &created, &updated)
	c.Reviewed = reviewed == 1
	c.ExpiresAt = fromNullMs(exp)
	c.CreatedAt, c.UpdatedAt = fromMs(created), fromMs(updated)
	return c, err
}

func (t *txn) InsertContext(c domain.Context) error {
	_, err := t.tx.Exec(`insert into contexts(`+ctxCols+`) values(?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.NamespaceID, c.Type, c.Status, c.CurrentVersion, c.HeadTrust, boolInt(c.Reviewed),
		msPtr(c.ExpiresAt), c.SupersededBy, ms(c.CreatedAt), ms(c.UpdatedAt))
	return err
}

func (t *txn) UpdateContext(c domain.Context) error {
	res, err := t.tx.Exec(`update contexts set status=?, current_version=?, head_trust=?, reviewed=?,
		expires_at=?, superseded_by=?, updated_at=? where id=?`,
		c.Status, c.CurrentVersion, c.HeadTrust, boolInt(c.Reviewed), msPtr(c.ExpiresAt),
		c.SupersededBy, ms(c.UpdatedAt), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (t *txn) GetContext(id string) (domain.Context, error) {
	c, err := scanContext(t.tx.QueryRow(`select `+ctxCols+` from contexts where id=?`, id))
	return c, notFound(err)
}

func (t *txn) ListContexts(f domain.ContextFilter) ([]domain.Context, error) {
	q := `select ` + ctxCols + ` from contexts where 1=1`
	var args []any
	if len(f.NamespaceIDs) > 0 {
		q += ` and namespace_id in (` + placeholders(len(f.NamespaceIDs)) + `)`
		for _, v := range f.NamespaceIDs {
			args = append(args, v)
		}
	}
	if len(f.Types) > 0 {
		q += ` and type in (` + placeholders(len(f.Types)) + `)`
		for _, v := range f.Types {
			args = append(args, v)
		}
	}
	if len(f.Statuses) > 0 {
		q += ` and status in (` + placeholders(len(f.Statuses)) + `)`
		for _, v := range f.Statuses {
			args = append(args, v)
		}
	}
	if f.Unreviewed {
		q += ` and reviewed = 0`
	}
	if f.NotExpiredAt != nil {
		q += ` and (expires_at is null or expires_at > ?)`
		args = append(args, ms(*f.NotExpiredAt))
	}
	q += ` order by updated_at desc, id`
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Context
	for rows.Next() {
		c, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (t *txn) SearchContexts(f domain.SearchFilter) ([]domain.SearchHit, error) {
	if f.Match == "" || len(f.NamespaceIDs) == 0 {
		return nil, nil
	}
	cols := "c." + strings.ReplaceAll(ctxCols, ", ", ", c.")
	// bm25 is lower for better matches; titles weigh 5x the body.
	q := `select ` + cols + `, -bm25(contexts_fts, 5.0, 1.0)
		from contexts_fts join contexts c on c.rowid = contexts_fts.rowid
		where contexts_fts match ? and c.status = 'active'
		and (c.expires_at is null or c.expires_at > ?)
		and c.namespace_id in (` + placeholders(len(f.NamespaceIDs)) + `)`
	args := []any{f.Match, ms(f.NotExpiredAt)}
	for _, v := range f.NamespaceIDs {
		args = append(args, v)
	}
	if len(f.Types) > 0 {
		q += ` and c.type in (` + placeholders(len(f.Types)) + `)`
		for _, v := range f.Types {
			args = append(args, v)
		}
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	q += ` order by bm25(contexts_fts, 5.0, 1.0) limit ?`
	args = append(args, limit)
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SearchHit
	for rows.Next() {
		var c domain.Context
		var reviewed int
		var exp sql.NullInt64
		var created, updated int64
		var score float64
		if err := rows.Scan(&c.ID, &c.NamespaceID, &c.Type, &c.Status, &c.CurrentVersion, &c.HeadTrust,
			&reviewed, &exp, &c.SupersededBy, &created, &updated, &score); err != nil {
			return nil, err
		}
		c.Reviewed, c.ExpiresAt = reviewed == 1, fromNullMs(exp)
		c.CreatedAt, c.UpdatedAt = fromMs(created), fromMs(updated)
		out = append(out, domain.SearchHit{Context: c, Lexical: score})
	}
	return out, rows.Err()
}

const revCols = `id, context_id, version, title, body, trust, author_kind, author_id, proposal_id, created_at`

func scanRevision(r interface{ Scan(...any) error }) (domain.Revision, error) {
	var v domain.Revision
	var c int64
	err := r.Scan(&v.ID, &v.ContextID, &v.Version, &v.Title, &v.Body, &v.Trust, &v.AuthorKind, &v.AuthorID, &v.ProposalID, &c)
	v.CreatedAt = fromMs(c)
	return v, err
}

func (t *txn) InsertRevision(v domain.Revision) error {
	_, err := t.tx.Exec(`insert into revisions(`+revCols+`) values(?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.ContextID, v.Version, v.Title, v.Body, v.Trust, v.AuthorKind, v.AuthorID, v.ProposalID, ms(v.CreatedAt))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return domain.ErrStaleVersion
	}
	return err
}

func (t *txn) GetRevision(contextID string, version int) (domain.Revision, error) {
	v, err := scanRevision(t.tx.QueryRow(`select `+revCols+` from revisions where context_id=? and version=?`, contextID, version))
	return v, notFound(err)
}

func (t *txn) ListRevisions(contextID string) ([]domain.Revision, error) {
	rows, err := t.tx.Query(`select `+revCols+` from revisions where context_id=? order by version`, contextID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Revision
	for rows.Next() {
		v, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (t *txn) InsertRelationship(fromID, toID, kind string, at time.Time) error {
	_, err := t.tx.Exec(`insert into relationships(from_id, to_id, kind, created_at) values(?,?,?,?)`, fromID, toID, kind, ms(at))
	return err
}

// ---- proposals

const propCols = `id, namespace_id, kind, origin, status, target_context_id, base_version, type, title, body,
	trust, agent_id, idempotency_key, result_context_id, decision_note, created_at, decided_at, decided_by`

func scanProposal(r interface{ Scan(...any) error }) (domain.Proposal, error) {
	var p domain.Proposal
	var created int64
	var decided sql.NullInt64
	err := r.Scan(&p.ID, &p.NamespaceID, &p.Kind, &p.Origin, &p.Status, &p.TargetContextID, &p.BaseVersion,
		&p.Type, &p.Title, &p.Body, &p.Trust, &p.AgentID, &p.IdempotencyKey, &p.ResultContextID,
		&p.DecisionNote, &created, &decided, &p.DecidedBy)
	p.CreatedAt = fromMs(created)
	p.DecidedAt = fromNullMs(decided)
	return p, err
}

func (t *txn) InsertProposal(p domain.Proposal) error {
	_, err := t.tx.Exec(`insert into proposals(`+propCols+`) values(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.NamespaceID, p.Kind, p.Origin, p.Status, p.TargetContextID, p.BaseVersion, p.Type, p.Title, p.Body,
		p.Trust, p.AgentID, p.IdempotencyKey, p.ResultContextID, p.DecisionNote, ms(p.CreatedAt), msPtr(p.DecidedAt), p.DecidedBy)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return domain.ErrConflict
	}
	return err
}

func (t *txn) UpdateProposal(p domain.Proposal) error {
	res, err := t.tx.Exec(`update proposals set status=?, result_context_id=?, decision_note=?, decided_at=?, decided_by=? where id=?`,
		p.Status, p.ResultContextID, p.DecisionNote, msPtr(p.DecidedAt), p.DecidedBy, p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (t *txn) GetProposal(id string) (domain.Proposal, error) {
	p, err := scanProposal(t.tx.QueryRow(`select `+propCols+` from proposals where id=?`, id))
	return p, notFound(err)
}

func (t *txn) GetProposalByIdempotencyKey(agentID, key string) (domain.Proposal, error) {
	p, err := scanProposal(t.tx.QueryRow(`select `+propCols+` from proposals where agent_id=? and idempotency_key=?`, agentID, key))
	return p, notFound(err)
}

func (t *txn) ListProposals(f domain.ProposalFilter) ([]domain.Proposal, error) {
	q := `select ` + propCols + ` from proposals where 1=1`
	var args []any
	if len(f.Statuses) > 0 {
		q += ` and status in (` + placeholders(len(f.Statuses)) + `)`
		for _, v := range f.Statuses {
			args = append(args, v)
		}
	}
	if f.AgentID != "" {
		q += ` and agent_id = ?`
		args = append(args, f.AgentID)
	}
	q += ` order by created_at, id`
	rows, err := t.tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (t *txn) CountPendingProposals(namespaceID string) (int, error) {
	var n int
	err := t.tx.QueryRow(`select count(*) from proposals where status = 'pending' and namespace_id = ?`, namespaceID).Scan(&n)
	return n, err
}

// ---- agents and grants

const agentCols = `id, name, client, token_hash, created_at, last_seen_at, revoked_at`

func scanAgent(r interface{ Scan(...any) error }) (domain.Agent, error) {
	var a domain.Agent
	var created int64
	var seen, revoked sql.NullInt64
	err := r.Scan(&a.ID, &a.Name, &a.Client, &a.TokenHash, &created, &seen, &revoked)
	a.CreatedAt = fromMs(created)
	a.LastSeenAt, a.RevokedAt = fromNullMs(seen), fromNullMs(revoked)
	return a, err
}

func (t *txn) InsertAgent(a domain.Agent) error {
	_, err := t.tx.Exec(`insert into agents(`+agentCols+`) values(?,?,?,?,?,?,?)`,
		a.ID, a.Name, a.Client, a.TokenHash, ms(a.CreatedAt), msPtr(a.LastSeenAt), msPtr(a.RevokedAt))
	return err
}

func (t *txn) UpdateAgent(a domain.Agent) error {
	res, err := t.tx.Exec(`update agents set name=?, last_seen_at=?, revoked_at=? where id=?`,
		a.Name, msPtr(a.LastSeenAt), msPtr(a.RevokedAt), a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (t *txn) GetAgent(id string) (domain.Agent, error) {
	a, err := scanAgent(t.tx.QueryRow(`select `+agentCols+` from agents where id=?`, id))
	return a, notFound(err)
}

func (t *txn) GetAgentByTokenHash(hash string) (domain.Agent, error) {
	a, err := scanAgent(t.tx.QueryRow(`select `+agentCols+` from agents where token_hash=?`, hash))
	return a, notFound(err)
}

func (t *txn) ListAgents() ([]domain.Agent, error) {
	rows, err := t.tx.Query(`select ` + agentCols + ` from agents order by created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (t *txn) InsertGrant(g domain.Grant) error {
	_, err := t.tx.Exec(`insert or ignore into grants(agent_id, namespace_id) values(?,?)`, g.AgentID, g.NamespaceID)
	return err
}

func (t *txn) ListGrants(agentID string) ([]domain.Grant, error) {
	rows, err := t.tx.Query(`select agent_id, namespace_id from grants where agent_id=?`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Grant
	for rows.Next() {
		var g domain.Grant
		if err := rows.Scan(&g.AgentID, &g.NamespaceID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---- audit, outbox, usage

func (t *txn) AppendAudit(e domain.AuditEvent) error {
	_, err := t.tx.Exec(`insert into audit_events(id, at, actor_kind, actor_id, action, subject, detail) values(?,?,?,?,?,?,?)`,
		e.ID, ms(e.At), e.ActorKind, e.ActorID, e.Action, e.Subject, e.Detail)
	return err
}

func (t *txn) ListAudit(limit int) ([]domain.AuditEvent, error) {
	rows, err := t.tx.Query(`select id, at, actor_kind, actor_id, action, subject, detail from audit_events order by at desc, id desc limit ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var e domain.AuditEvent
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.ActorKind, &e.ActorID, &e.Action, &e.Subject, &e.Detail); err != nil {
			return nil, err
		}
		e.At = fromMs(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (t *txn) AppendOutbox(o domain.OutboxEntry) error {
	_, err := t.tx.Exec(`insert into outbox(id, idempotency_key, op, entity, entity_id, payload, created_at) values(?,?,?,?,?,?,?)`,
		o.ID, o.IdempotencyKey, o.Op, o.Entity, o.EntityID, o.Payload, ms(o.CreatedAt))
	return err
}

func (t *txn) CountOutbox() (int, error) {
	var n int
	err := t.tx.QueryRow(`select count(*) from outbox`).Scan(&n)
	return n, err
}

func (t *txn) CaptureStats() ([]domain.CaptureStat, error) {
	rows, err := t.tx.Query(`
select p.origin, p.status, count(*),
  coalesce(sum(exists(select 1 from usage_events u where u.context_id = p.result_context_id and u.kind = 'retrieved' and u.agent_id != p.agent_id)), 0),
  coalesce(sum(exists(select 1 from usage_events u where u.context_id = p.result_context_id and u.kind = 'used')), 0),
  coalesce(sum(exists(select 1 from usage_events u where u.context_id = p.result_context_id and u.kind = 'used' and u.agent_id != p.agent_id)), 0)
from proposals p
where p.kind = 'create'
group by p.origin, p.status
order by p.origin, p.status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CaptureStat
	for rows.Next() {
		var s domain.CaptureStat
		if err := rows.Scan(&s.Origin, &s.Status, &s.Proposals, &s.RetrievedByOther, &s.Used, &s.UsedByOther); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (t *txn) RecordUsage(u domain.UsageEvent) error {
	_, err := t.tx.Exec(`insert into usage_events(id, at, agent_id, context_id, version, kind) values(?,?,?,?,?,?)`,
		u.ID, ms(u.At), u.AgentID, u.ContextID, u.Version, u.Kind)
	return err
}
