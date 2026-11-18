package sqlite

// SchemaVersion is the schema this binary writes. A process refuses to open a
// database with a newer version (ADR-016).
const SchemaVersion = 2

// migrations[i] upgrades the schema from version i to i+1.
var migrations = []string{
	// v0 -> v1
	`
create table users (
  id          text primary key,
  name        text not null,
  created_at  integer not null
);

create table namespaces (
  id            text primary key,
  parent_id     text not null default '',
  name          text not null,
  storage_mode  text not null check (storage_mode in ('local_only','local_primary','cloud_primary','shared')),
  created_at    integer not null,
  unique (parent_id, name)
);

create table namespace_aliases (
  identity      text primary key,
  namespace_id  text not null references namespaces(id)
);

create table contexts (
  id               text primary key,
  namespace_id     text not null references namespaces(id),
  type             text not null,
  status           text not null check (status in ('active','superseded','deleted')),
  current_version  integer not null check (current_version >= 1),
  head_trust       text not null,
  reviewed         integer not null,
  expires_at       integer,
  superseded_by    text not null default '',
  created_at       integer not null,
  updated_at       integer not null
);
create index contexts_namespace on contexts(namespace_id, status);

create table revisions (
  id           text primary key,
  context_id   text not null references contexts(id),
  version      integer not null,
  title        text not null,
  body         text not null,
  trust        text not null,
  author_kind  text not null,
  author_id    text not null,
  proposal_id  text not null default '',
  created_at   integer not null,
  unique (context_id, version)
);
-- Invariant 3: no revision overwrite.
create trigger revisions_no_update before update on revisions
begin select raise(abort, 'revisions are immutable'); end;
create trigger revisions_no_delete before delete on revisions
begin select raise(abort, 'revisions are immutable'); end;

create table relationships (
  from_id     text not null references contexts(id),
  to_id       text not null references contexts(id),
  kind        text not null check (kind in ('supersedes','depends_on','refines','contradicts','relates_to')),
  created_at  integer not null,
  primary key (from_id, to_id, kind)
);

create table proposals (
  id                 text primary key,
  namespace_id       text not null references namespaces(id),
  kind               text not null,
  origin             text not null,
  status             text not null check (status in ('pending','accepted','rejected')),
  target_context_id  text not null default '',
  base_version       integer not null default 0,
  type               text not null default '',
  title              text not null,
  body               text not null,
  trust              text not null,
  agent_id           text not null default '',
  idempotency_key    text not null default '',
  result_context_id  text not null default '',
  decision_note      text not null default '',
  created_at         integer not null,
  decided_at         integer,
  decided_by         text not null default ''
);
create index proposals_status on proposals(status);
create unique index proposals_idempotency on proposals(agent_id, idempotency_key) where idempotency_key != '';

create table agents (
  id            text primary key,
  name          text not null,
  client        text not null,
  token_hash    text not null unique,
  created_at    integer not null,
  last_seen_at  integer,
  revoked_at    integer
);

create table grants (
  agent_id      text not null references agents(id),
  namespace_id  text not null default '',
  primary key (agent_id, namespace_id)
);

create table audit_events (
  id          text primary key,
  at          integer not null,
  actor_kind  text not null,
  actor_id    text not null,
  action      text not null,
  subject     text not null,
  detail      text not null
);
create trigger audit_no_update before update on audit_events
begin select raise(abort, 'audit events are append-only'); end;
create trigger audit_no_delete before delete on audit_events
begin select raise(abort, 'audit events are append-only'); end;

-- Invariant 4: every acknowledged local write has an outbox entry in the same transaction.
create table outbox (
  id               text primary key,
  idempotency_key  text not null unique,
  op               text not null,
  entity           text not null,
  entity_id        text not null,
  payload          text not null,
  created_at       integer not null
);

create table usage_events (
  id          text primary key,
  at          integer not null,
  agent_id    text not null,
  context_id  text not null,
  version     integer not null,
  kind        text not null check (kind in ('retrieved','used'))
);
`,
	// v1 -> v2: full-text search over each context's head revision (0.2).
	`
create virtual table contexts_fts using fts5(title, body, tokenize = 'porter unicode61');

-- The newest revision is always the head, so re-index on every revision insert.
-- The FTS rowid is the context's rowid.
create trigger revisions_fts_index after insert on revisions
begin
  delete from contexts_fts where rowid = (select rowid from contexts where id = new.context_id);
  insert into contexts_fts(rowid, title, body)
    values ((select rowid from contexts where id = new.context_id), new.title, new.body);
end;

insert into contexts_fts(rowid, title, body)
  select c.rowid, r.title, r.body
  from contexts c join revisions r on r.context_id = c.id and r.version = c.current_version;
`,
}
