<div align="center">

# Centrext

**One shared, governed memory for all your AI tools.**

Explain something once to Claude Code. Cursor and Claude Desktop already know it.

![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)
![Status](https://img.shields.io/badge/status-alpha-orange)
![Storage](https://img.shields.io/badge/storage-local%20%C2%B7%20encrypted-2ea44f)
![Protocol](https://img.shields.io/badge/protocol-MCP-6E56CF)

[Quick start](#-quick-start) · [How it works](#-how-it-works) · [Trust](#-trust-and-review) · [Search](#-search) · [CLI](#-cli) · [Security](#-security)

</div>

---

## ✨ Why

Every AI tool keeps its own memory, so you repeat yourself, decisions get lost, and agents work from different assumptions.

Centrext is one local store that every connected agent reads from and writes to, with rules about who may change what. A `CLAUDE.md` file is shared too, so here is what Centrext adds:

| | Markdown file in the repo | Centrext |
|---|---|---|
| **Clients** | Each tool reads its own file | Claude Code, Cursor and Claude Desktop share one store |
| **Plain chats** | No repo, no file | Your Personal namespace follows you |
| **Trust** | Every line looks equally true | Verified, session-confirmed and agent-noticed are kept apart |
| **History** | Git history of one file | Immutable revisions, with the agent that wrote each one |
| **Access** | Anyone with the file | One revocable token per client, scoped to namespaces |

## 🚀 Quick start

Requires Go 1.26 or newer. No C toolchain is needed, because SQLite is pure Go and encrypted.

```bash
go install ./cmd/centrext
```

```bash
centrext init
```

Connect your clients:

```bash
centrext connect claude-code
```

```bash
centrext connect claude-desktop
```

Cursor is connected per project, so run this inside each project:

```bash
centrext connect cursor
```

Check the setup, including tokens accidentally committed to git:

```bash
centrext doctor
```

Then start a new session and say *"remember that orders live in PostgreSQL"*. Open another client and ask which database to use.

## 🧭 How it works

```mermaid
flowchart LR
    CC[Claude Code] -- MCP --> C((centrext))
    CU[Cursor] -- MCP --> C
    CD[Claude Desktop] -- MCP --> C
    C --> DB[(Encrypted SQLite<br/>~/.centrext)]
    C -. session summary .-> CC
    C -. rules file .-> CU
    C -. server instructions .-> CD
```

1. **Save.** An agent saves something when you say "remember this", or after asking you once a choice is settled.
2. **Govern.** Centrext rejects secrets, sets the trust level from *how* the item arrived (never from what the agent claims), and stores an immutable revision.
3. **Deliver.** Every session starts with a short summary of the project's decisions and conventions. Agents search for more before they pick a schema, an API or a dependency.

Projects are recognized from the git remote, so SSH and HTTPS clones and worktrees share context. Chats without a folder use your Personal namespace.

## 🛡️ Trust and review

| Trust | How an item gets it |
|---|---|
| `user_verified` | You approved it outside the chat, with `centrext review` |
| `session_confirmed` | You asked an agent to remember it, or agreed when it asked |
| `agent_reported` | An agent noticed it without asking. Never in summaries, and expires after 30 days |

Agents can't raise trust, delete context, or change anything you verified. Those actions wait for you:

```bash
centrext review
```

## 🔎 Search

Agents search with `context_search`, and you can use `centrext context search`. Search is full-text by default, ranked by trust, and never returns superseded or deleted items.

<details>
<summary><b>Semantic search</b>: find paraphrases through a hosted embeddings API (optional)</summary>

<br/>

```bash
centrext embeddings enable --provider openai
```

```bash
centrext embeddings key
```

Providers are `openai`, `voyage`, or any `openai-compatible` endpoint (with `--base-url` and `--model`). The key is kept in the keychain; `CENTREXT_EMBEDDINGS_KEY` also works.

Embedding sends text to the provider, so it only covers namespaces whose policy allows content to leave the device. New namespaces are `local_only`, and stay full-text until you opt one in:

```bash
centrext namespace policy NAMESPACE local_primary
```

Vectors are computed during searches, or all at once with `centrext embeddings sync`. If the API is slow or down, search falls back to full-text. Queries that contain secrets are never sent.

</details>

<details>
<summary><b>Per-prompt context</b>: give Claude Code the right decision before it asks (opt-in experiment)</summary>

<br/>

```bash
centrext connect claude-code --prompt-context on
```

Each prompt is matched against saved context **on your machine**, and the best clear match is added to what Claude sees. It's tuned for precision: an item must match several words of the prompt, only reviewed or confirmed items qualify, nothing is repeated within a session, and the prompt is never stored or sent anywhere. Turn it off with `--prompt-context off`.

</details>

## 💻 CLI

| Command | What it does |
|---|---|
| `init` | Create the encrypted store and your Personal namespace |
| `connect` / `disconnect CLIENT` | Set up or remove `claude-code`, `cursor` or `claude-desktop` |
| `doctor` | Check the store, clients, generated files and repos for leaked tokens |
| `store status / reencrypt` | Show the store's encryption, or upgrade an older store to AES-XTS |
| `review [approve / reject ID]` | Approve or reject what agents proposed |
| `context search / list / show / history / add / verify / delete` | Find and manage saved context |
| `namespace list / create / link / policy` | Manage namespaces, their identities and storage policy |
| `link NAMESPACE` | Pin a repo to a namespace with a `.centrext` file |
| `export` | Write the repo's verified decisions to `.centrext-export/` for teammates |
| `embeddings enable / key / status / sync / disable` | Optional semantic search |
| `stats` | See what gets saved, and how often other agents retrieve and use it |
| `agent register / list / grant / revoke` | Manage agents and their access |
| `mcp`, `hook` | Started by your clients, not by you |

## 🔐 Security

- **Encrypted at rest.** AES-XTS page encryption, with the key in the OS keychain. On machines without a keychain, such as headless Linux and WSL, keys go to files only you can read.
- **Durable.** Every acknowledged write is flushed to disk, including on macOS.
- **No tokens in config files.** Each client's token is kept in the keychain, and configs only say `--client cursor`. Centrext stores only token hashes.
- **Secrets are rejected.** Writes that contain keys, tokens or passwords are refused.
- **Instructions aren't trusted.** Text an agent read can never become trusted memory by itself.
- **Local by default.** New namespaces are `local_only`. Nothing leaves the device until you change a namespace's policy.
- **Limit:** local agent identity gives attribution and revocation. It isn't a boundary against code running as your OS user.

## 📊 Measured, not guessed

Ranking changes are judged on a labeled set in `internal/eval`: 30 items, 60 search queries and 34 prompts. The set is synthetic, so treat these numbers as a baseline.

| | Result |
|---|---|
| Search, all queries | 83% precision@5, 90% recall@5, 0 stale results |
| Search, paraphrased queries | 49% precision@5, 64% recall@5, which is the gap embeddings target |
| Per-prompt context | 93% precision; silent on all 14 prompts with no answer |

## 🗺️ Roadmap

- [x] **0.1 Local runtime:** encrypted store, namespaces, immutable revisions, proposals, agents
- [ ] **0.2 Multi-agent:** MCP server; Claude Code, Cursor and Claude Desktop; `connect`, `doctor` *(built; the real-client hit-rate check is left)*
- [ ] **0.3 Context intelligence:** eval set, hosted embeddings, hybrid ranking, per-prompt context *(in progress)*
- [ ] **0.4 Hybrid sync:** cloud API, shared namespaces, web chat apps
- [ ] **0.5 Enterprise:** org namespaces, RBAC, audit, retention

## 🛠️ Develop

```bash
go test ./...
```

```bash
go test ./internal/eval -v
```

<details>
<summary>Project layout</summary>

```
cmd/centrext              CLI, MCP server and hook entry points
internal/domain           entities, validation, invariants (no I/O)
internal/policy           who may do what; secret detection
internal/application      use cases shared by every transport
internal/retrieval        query building, ranking, token budgets
internal/workspace        project identity from a folder (marker, git)
internal/transport/mcp    MCP tools, resources and prompts
internal/connect          client setup, Cursor rules file, doctor
internal/session          what hooks already delivered in a session
internal/infrastructure   encrypted SQLite, OS keychain, embeddings client
internal/eval             labeled retrieval set and metrics
```

</details>

<details>
<summary>Recording embedding vectors for the eval</summary>

<br/>

Hybrid results are measured with vectors recorded once from a provider and replayed offline. Recording sends only the synthetic eval set:

```bash
CENTREXT_EVAL_RECORD=openai OPENAI_API_KEY=... CENTREXT_EVAL_SWEEP=1 go test ./internal/eval -run Hybrid -v
```

</details>

Data lives in `~/.centrext`. Set `CENTREXT_HOME` to use another folder.
