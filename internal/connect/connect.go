package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"centrext/internal/application"
	"centrext/internal/domain"
	mcpx "centrext/internal/transport/mcp"
	"centrext/internal/workspace"
)

// Supported clients.
const (
	ClaudeCode    = "claude-code"
	Cursor        = "cursor"
	ClaudeDesktop = "claude-desktop"
)

// Clients lists the clients connect supports.
var Clients = []string{ClaudeCode, Cursor, ClaudeDesktop}

// ServerName is the MCP server name written into client configs.
const ServerName = "centrext"

// TokenStore keeps agent tokens outside config files (the OS keychain).
type TokenStore interface {
	Store(agentID, token string) error
	Load(agentID string) (string, error)
	Delete(agentID string) error
}

// Env is everything connect touches outside the database. Tests point it at
// temp folders and fakes.
type Env struct {
	UserHome     string // ~
	ConfigDir    string // os.UserConfigDir(): ~/Library/Application Support on macOS
	CentrextHome string
	Binary       string // absolute path of the centrext binary to run
	Hostname     string
	Tokens       TokenStore
	// Run runs an external command (the `claude` CLI). Default: os/exec.
	Run func(name string, args ...string) ([]byte, error)
}

func (e Env) run(name string, args ...string) ([]byte, error) {
	if e.Run != nil {
		return e.Run(name, args...)
	}
	return exec.Command(name, args...).CombinedOutput()
}

// Options selects what to connect.
type Options struct {
	Client     string
	Dir        string   // project folder (Cursor); default: repo root of the working folder
	Namespaces []string // restrict a new agent to these namespace IDs; default: all
}

// Report says what connect did and what the user still has to do.
type Report struct {
	Client   string
	AgentID  string
	NewAgent bool
	Project  string // Cursor project folder
	Changed  []string
	Manual   []string
	Notes    []string
}

// Connect installs Centrext into one client. Running it again repairs the
// install and reuses the agent while its token still works.
func Connect(ctx context.Context, svc *application.Service, user domain.Actor, env Env, opts Options) (Report, error) {
	rep := Report{Client: opts.Client}
	if !known(opts.Client) {
		return rep, fmt.Errorf("unknown client %q; supported: %s", opts.Client, strings.Join(Clients, ", "))
	}
	if env.Binary == "" {
		return rep, errors.New("can't find the centrext binary path")
	}
	st, err := LoadState(env.CentrextHome)
	if err != nil {
		return rep, err
	}
	agent, err := ensureAgent(ctx, svc, user, env, &st, opts, &rep)
	if err != nil {
		return rep, err
	}
	switch opts.Client {
	case ClaudeCode:
		err = installClaudeCode(env, &rep)
	case ClaudeDesktop:
		err = installClaudeDesktop(env, &rep)
	case Cursor:
		err = installCursor(ctx, svc, user, env, &st, agent, opts, &rep)
	}
	if err != nil {
		return rep, err
	}
	return rep, st.save(env.CentrextHome)
}

func known(client string) bool {
	for _, c := range Clients {
		if c == client {
			return true
		}
	}
	return false
}

// ensureAgent reuses the client's agent if its stored token still
// authenticates; otherwise it registers a new one and revokes the old one,
// whose token is lost (ADR-018).
func ensureAgent(ctx context.Context, svc *application.Service, user domain.Actor, env Env, st *State, opts Options, rep *Report) (domain.Actor, error) {
	if old, ok := st.Agents[opts.Client]; ok {
		if tok, err := env.Tokens.Load(old); err == nil {
			if a, err := svc.Authenticate(ctx, tok); err == nil && a.ID == old {
				for _, ns := range opts.Namespaces {
					if err := svc.GrantAgent(ctx, user, old, ns); err != nil {
						return a, err
					}
				}
				rep.AgentID = old
				return a, nil
			}
		}
		if ag, err := svc.GetAgent(ctx, user, old); err == nil && ag.Active() {
			if _, err := svc.RevokeAgent(ctx, user, old, false); err != nil {
				return domain.Actor{}, err
			}
			rep.Notes = append(rep.Notes, "Revoked the previous agent "+old+": its token was missing or invalid.")
		}
	}
	name := opts.Client + "@" + env.Hostname
	if len(name) > 100 {
		name = name[:100]
	}
	ag, token, err := svc.RegisterAgent(ctx, user, application.RegisterAgentInput{Name: name, Client: opts.Client,
		NamespaceIDs: opts.Namespaces, AllNamespaces: len(opts.Namespaces) == 0})
	if err != nil {
		return domain.Actor{}, err
	}
	if err := env.Tokens.Store(ag.ID, token); err != nil {
		_, _ = svc.RevokeAgent(ctx, user, ag.ID, false)
		return domain.Actor{}, fmt.Errorf("couldn't store the agent token in the OS keychain (%v). Until the keychain fallback exists (ADR-021), register an agent with `centrext agent register --print-token` and set CENTREXT_TOKEN in the client's user-level config", err)
	}
	st.Agents[opts.Client] = ag.ID
	rep.AgentID, rep.NewAgent = ag.ID, true
	return svc.Authenticate(ctx, token)
}

// ---- Claude Code: MCP server through the `claude` CLI, SessionStart hook.

func installClaudeCode(env Env, rep *Report) error {
	server, _ := json.Marshal(map[string]any{"type": "stdio", "command": env.Binary, "args": []string{"mcp", "--client", ClaudeCode}})
	add := []string{"mcp", "add-json", "--scope", "user", ServerName, string(server)}
	_, _ = env.run("claude", "mcp", "remove", "--scope", "user", ServerName) // fine if absent
	if out, err := env.run("claude", add...); err != nil {
		if !errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("claude mcp add-json failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
		rep.Manual = append(rep.Manual, "The `claude` CLI isn't on PATH. Add the MCP server with:\nclaude "+joinShell(add))
	} else {
		rep.Changed = append(rep.Changed, "Claude Code user MCP config (claude mcp add-json --scope user)")
	}
	settings := filepath.Join(env.UserHome, ".claude", "settings.json")
	cmd := shellQuote(env.Binary) + " hook session-start --client " + ClaudeCode
	changed, err := setClaudeHook(settings, cmd, true)
	if err != nil {
		return err
	}
	if changed {
		rep.Changed = append(rep.Changed, settings)
	}
	rep.Notes = append(rep.Notes, "Start a new Claude Code session to load it.")
	return nil
}

func joinShell(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func isOurHook(cmd any) bool {
	s, _ := cmd.(string)
	return strings.Contains(s, "centrext") && strings.Contains(s, "hook session-start")
}

// setClaudeHook removes Centrext's SessionStart hooks from a Claude Code
// settings file and, with install, adds one running command. Other settings
// and hooks are kept.
func setClaudeHook(path, command string, install bool) (bool, error) {
	m, err := readJSON(path)
	if err != nil {
		return false, err
	}
	before, _ := json.Marshal(m)
	hooks, err := object(m, "hooks")
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	groups, ok := hooks["SessionStart"].([]any)
	if !ok && hooks["SessionStart"] != nil {
		return false, fmt.Errorf("%s: hooks.SessionStart is not a list", path)
	}
	var kept []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		if !ok || len(hs) == 0 {
			kept = append(kept, g)
			continue
		}
		var keep []any
		for _, h := range hs {
			if hm, ok := h.(map[string]any); ok && isOurHook(hm["command"]) {
				continue
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 {
			continue
		}
		gm["hooks"] = keep
		kept = append(kept, gm)
	}
	if install {
		kept = append(kept, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 15}}})
	}
	if len(kept) == 0 {
		delete(hooks, "SessionStart")
	} else {
		hooks["SessionStart"] = kept
	}
	if len(hooks) == 0 {
		delete(m, "hooks")
	}
	after, _ := json.Marshal(m)
	if string(before) == string(after) {
		return false, nil
	}
	return true, writeJSON(path, m, 0o600)
}

// ---- Claude Desktop: user-level MCP config. Chats use the Personal namespace
// and get the summary through the server instructions (ADR-019).

func claudeDesktopConfig(env Env) string {
	return filepath.Join(env.ConfigDir, "Claude", "claude_desktop_config.json")
}

func installClaudeDesktop(env Env, rep *Report) error {
	path := claudeDesktopConfig(env)
	changed, err := setServer(path, map[string]any{"command": env.Binary, "args": []any{"mcp", "--client", ClaudeDesktop}}, 0o600)
	if err != nil {
		return err
	}
	if changed {
		rep.Changed = append(rep.Changed, path)
	}
	rep.Notes = append(rep.Notes, "Quit and reopen Claude Desktop to load it.")
	return nil
}

// setServer sets (or, with nil, removes) mcpServers.centrext in a config file.
func setServer(path string, server map[string]any, perm os.FileMode) (bool, error) {
	m, err := readJSON(path)
	if err != nil {
		return false, err
	}
	before, _ := json.Marshal(m)
	servers, err := object(m, "mcpServers")
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if server == nil {
		delete(servers, ServerName)
	} else {
		servers[ServerName] = server
	}
	after, _ := json.Marshal(m)
	if string(before) == string(after) {
		return false, nil
	}
	return true, writeJSON(path, m, perm)
}

// ---- Cursor: project MCP config (for ${workspaceFolder}) and a generated
// rules file, Cursor's main delivery route (ADR-014, docs/clients.md).

// RulesFile is the generated rules file, relative to the project folder.
var RulesFile = filepath.Join(".cursor", "rules", "centrext.mdc")

func projectDir(dir string) (string, error) {
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if root := workspace.RepoRoot(dir); root != "" {
		return root, nil
	}
	return dir, nil
}

func installCursor(ctx context.Context, svc *application.Service, user domain.Actor, env Env, st *State, agent domain.Actor, opts Options, rep *Report) error {
	dir, err := projectDir(opts.Dir)
	if err != nil {
		return err
	}
	rep.Project = dir
	id, err := workspace.Resolve(dir)
	if err != nil {
		return err
	}
	res, err := svc.ResolveNamespace(ctx, agent, id)
	if err != nil {
		return err
	}
	if id.Kind == workspace.KindNone {
		rep.Notes = append(rep.Notes, dir+" isn't in a git repo, so Cursor will use your Personal namespace there. Pin another with `centrext link NAMESPACE`.")
	}

	// The project file names no agent and holds no token, only `--client`, so
	// it's safe to commit (ADR-018). The binary path is this machine's.
	mcpPath := filepath.Join(dir, ".cursor", "mcp.json")
	changed, err := setServer(mcpPath, map[string]any{"command": env.Binary,
		"args": []any{"mcp", "--client", Cursor, "--workspace", "${workspaceFolder}"}}, 0o644)
	if err != nil {
		return err
	}
	if changed {
		rep.Changed = append(rep.Changed, mcpPath)
	}
	rules, err := writeRules(ctx, svc, agent, dir, res.Namespace.ID)
	if err != nil {
		return err
	}
	if rules {
		rep.Changed = append(rep.Changed, filepath.Join(dir, RulesFile))
	}
	if added, err := ensureGitignore(dir, filepath.ToSlash(RulesFile)); err != nil {
		return err
	} else if added {
		rep.Changed = append(rep.Changed, filepath.Join(dir, ".gitignore"))
	}
	st.CursorProjects[dir] = res.Namespace.ID
	rep.Notes = append(rep.Notes,
		"Namespace for this project: "+res.Namespace.Name+" ("+res.Namespace.ID+").",
		".cursor/mcp.json holds this machine's path to centrext; commit it only if teammates share that path.",
		"Reload the Cursor window to load it.")
	return nil
}

// RenderRules builds the Cursor rules file from a session summary.
func RenderRules(summary, guide string) []byte {
	var b strings.Builder
	b.WriteString("---\ndescription: Shared context from Centrext (generated)\nalwaysApply: true\n---\n")
	b.WriteString("<!-- Generated by centrext and rewritten when shared context changes. Don't edit: change context through the agent or the `centrext` CLI. -->\n\n")
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n\n")
	b.WriteString(strings.TrimSpace(guide))
	b.WriteString("\n")
	return []byte(b.String())
}

func writeRules(ctx context.Context, svc *application.Service, agent domain.Actor, dir, nsID string) (bool, error) {
	sum, err := svc.Summary(ctx, agent, nsID, 1000)
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, RulesFile)
	data := RenderRules(sum, mcpx.Guide)
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) {
		return false, nil
	}
	return true, writeAtomic(path, data, 0o644)
}

// RefreshCursorRules rewrites the rules files of Cursor projects that show
// namespaceID ("" = all). It reads as the Cursor agent, so a rules file never
// shows more than Cursor itself could read; a revoked agent's files are left
// alone. Projects that no longer exist are skipped.
func RefreshCursorRules(ctx context.Context, svc *application.Service, centrextHome, namespaceID string) error {
	st, err := LoadState(centrextHome)
	if err != nil || len(st.CursorProjects) == 0 {
		return err
	}
	agentID, ok := st.Agents[Cursor]
	if !ok {
		return nil
	}
	user, err := svc.LocalUser(ctx)
	if err != nil {
		return err
	}
	agent, err := svc.ActAsAgent(ctx, user, agentID)
	if errors.Is(err, domain.ErrForbidden) || errors.Is(err, domain.ErrNotFound) {
		return nil // revoked or gone: leave its files until `disconnect`
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range sortedKeys(st.CursorProjects) {
		ns := st.CursorProjects[dir]
		if namespaceID != "" && ns != namespaceID {
			continue
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		if _, err := writeRules(ctx, svc, agent, dir, ns); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", dir, err))
		}
	}
	return errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- disconnect

// Disconnect removes Centrext from a client and revokes its agent. For
// Cursor it removes one project; the agent is revoked with the last project.
// Pending proposals stay in the review queue.
func Disconnect(ctx context.Context, svc *application.Service, user domain.Actor, env Env, opts Options) (Report, error) {
	rep := Report{Client: opts.Client}
	if !known(opts.Client) {
		return rep, fmt.Errorf("unknown client %q; supported: %s", opts.Client, strings.Join(Clients, ", "))
	}
	st, err := LoadState(env.CentrextHome)
	if err != nil {
		return rep, err
	}
	revoke := true
	switch opts.Client {
	case ClaudeCode:
		if out, err := env.run("claude", "mcp", "remove", "--scope", "user", ServerName); err != nil {
			if errors.Is(err, exec.ErrNotFound) {
				rep.Manual = append(rep.Manual, "Remove the MCP server with: claude mcp remove --scope user "+ServerName)
			} else if !strings.Contains(string(out), "No") { // "No MCP server found" is fine
				rep.Notes = append(rep.Notes, "claude mcp remove: "+strings.TrimSpace(string(out)))
			}
		} else {
			rep.Changed = append(rep.Changed, "Claude Code user MCP config")
		}
		settings := filepath.Join(env.UserHome, ".claude", "settings.json")
		if changed, err := setClaudeHook(settings, "", false); err != nil {
			return rep, err
		} else if changed {
			rep.Changed = append(rep.Changed, settings)
		}
	case ClaudeDesktop:
		path := claudeDesktopConfig(env)
		if _, err := os.Stat(path); err == nil {
			if changed, err := setServer(path, nil, 0o600); err != nil {
				return rep, err
			} else if changed {
				rep.Changed = append(rep.Changed, path)
			}
		}
	case Cursor:
		dir, err := projectDir(opts.Dir)
		if err != nil {
			return rep, err
		}
		rep.Project = dir
		mcpPath := filepath.Join(dir, ".cursor", "mcp.json")
		if _, err := os.Stat(mcpPath); err == nil {
			if changed, err := setServer(mcpPath, nil, 0o644); err != nil {
				return rep, err
			} else if changed {
				rep.Changed = append(rep.Changed, mcpPath)
			}
		}
		if err := os.Remove(filepath.Join(dir, RulesFile)); err == nil {
			rep.Changed = append(rep.Changed, filepath.Join(dir, RulesFile))
		}
		delete(st.CursorProjects, dir)
		revoke = len(st.CursorProjects) == 0
		if !revoke {
			rep.Notes = append(rep.Notes, fmt.Sprintf("Cursor stays connected in %d other project(s).", len(st.CursorProjects)))
		}
	}
	if id, ok := st.Agents[opts.Client]; ok && revoke {
		if ag, err := svc.GetAgent(ctx, user, id); err == nil && ag.Active() {
			if _, err := svc.RevokeAgent(ctx, user, id, false); err != nil {
				return rep, err
			}
		}
		_ = env.Tokens.Delete(id)
		delete(st.Agents, opts.Client)
		rep.AgentID = id
		rep.Notes = append(rep.Notes, "Revoked agent "+id+". Its pending proposals stay in `centrext review`.")
	}
	return rep, st.save(env.CentrextHome)
}
