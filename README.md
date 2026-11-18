# Centrext

**One shared, governed memory for all your AI tools.**

Explain something once to Claude Code, and Cursor or Claude Desktop knows it next time, without you repeating yourself.

![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)
![Status](https://img.shields.io/badge/status-0.2%20alpha-orange)
![Local first](https://img.shields.io/badge/storage-local%20%26%20encrypted-2ea44f)

---

## Why

Every AI tool keeps its own isolated memory. Decisions get lost between tools, and agents work from different assumptions.

Centrext is a local context store that every connected agent can read from and contribute to, with rules about who may change what. A `CLAUDE.md` file is shared too, but Centrext adds:

- **Every client:** coding agents and plain chats, with no file to copy between tools.
- **Trust levels:** what you approved, what was confirmed in a session, and what an agent only noticed.
- **Provenance and history:** immutable revisions, so you know which agent saved what and when.
- **Access control:** each client is a separate agent with its own token and namespaces, and can be revoked.

## How it works

```
Claude Code    ─┐
Cursor         ─┼─ MCP (stdio) ─►  centrext  ─►  encrypted SQLite (~/.centrext)
Claude Desktop ─┘                     │
                                      └─► session summary, through the Claude Code hook,
                                          the Cursor rules file or the MCP instructions
```

1. **Save.** An agent saves knowledge when you ask ("remember this"), or after asking you once a choice is settled.
2. **Govern.** Centrext checks for secrets, assigns a trust level from how the item arrived (never from what the agent claims), and stores an immutable revision.
3. **Deliver.** Each new session starts with a short summary of the project's decisions and conventions. Agents search for more before choosing schemas, APIs or dependencies.

Projects are recognized automatically from the git remote, so SSH and HTTPS clones and worktrees share the same context. Chats without a folder use your Personal namespace.

## Quick start

Requires Go 1.26+. No C toolchain is needed: SQLite is pure Go and encrypted.

```bash
go install ./cmd/centrext
```

```bash
centrext init
```

```bash
centrext connect claude-code
```

```bash
centrext connect claude-desktop
```

Run this inside each project you open in Cursor:

```bash
centrext connect cursor
```

Check everything, including tokens accidentally committed to git:

```bash
centrext doctor
```

Then start a new session and tell your agent to "remember" a decision. Open another client and ask about it.

## Trust and review

| Trust | How an item gets it |
|---|---|
| `user_verified` | You approved it outside the chat (`centrext review`) |
| `session_confirmed` | You asked an agent to remember it, or agreed when it asked |
| `agent_reported` | An agent noticed it without asking. Never in summaries; expires after 30 days |

Agents can't raise trust, delete context, or change anything you verified without your approval:

```bash
centrext review
```

## CLI

| Command | What it does |
|---|---|
| `init` | Create the encrypted store and your Personal namespace |
| `connect` / `disconnect CLIENT` | Install or remove Centrext for `claude-code`, `cursor` or `claude-desktop` |
| `doctor` | Check the store, clients, generated files and repos for leaked tokens |
| `stats` | See what gets saved, and how often other agents retrieve and use it |
| `context list / show / history / add / verify / delete` | Browse and manage saved context |
| `review [approve / reject ID]` | Approve or reject what agents proposed |
| `namespace list / create / link` | Manage namespaces and their identities |
| `link NAMESPACE` | Pin a repo to a namespace with a `.centrext` file |
| `agent register / list / grant / revoke` | Manage agents and their access |
| `mcp`, `hook session-start` | Started by clients, not by you |

## Security

- **Encrypted at rest.** The database key lives in the OS keychain.
- **Tokens stay out of config files.** One token per client is kept in the keychain, and configs only say `--client cursor`. Only token hashes are stored.
- **Secrets are rejected.** Writes that contain keys, tokens or passwords are refused.
- **Instructions aren't trusted.** Text an agent read can never become trusted memory by itself.
- **Limit:** local agent identity provides attribution and revocation. It isn't a boundary against code running as your OS user.

## Project layout

```
cmd/centrext              CLI, MCP server and hook entry points
internal/domain           entities, validation, invariants (no I/O)
internal/policy           who may do what; secret detection
internal/application      use cases shared by every transport
internal/retrieval        query building, ranking, token budgets
internal/workspace        project identity from a folder (marker, git)
internal/transport/mcp    MCP tools, resources and prompts
internal/connect          client installs, Cursor rules file, doctor
internal/infrastructure   encrypted SQLite store, OS keychain
```

## Roadmap

- [x] **0.1 Local runtime:** encrypted store, namespaces, immutable revisions, proposals, agents
- [ ] **0.2 Multi-agent:** MCP server, Claude Code, Cursor and Claude Desktop, search, `connect`, `doctor` *(in progress)*
- [ ] **0.3 Context intelligence:** embeddings, hybrid ranking, evaluation set
- [ ] **0.4 Hybrid sync:** cloud API, shared namespaces, web chat apps
- [ ] **0.5 Enterprise:** org namespaces, RBAC, audit, retention

## Develop

```bash
go test ./...
```

Measure retrieval quality (precision, recall, stale results) on the labeled set in `internal/eval`:

```bash
go test ./internal/eval -run Retrieval -v
```

Data lives in `~/.centrext`. Set `CENTREXT_HOME` to use another folder.
