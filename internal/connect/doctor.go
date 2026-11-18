package connect

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
	mcpx "centrext/internal/transport/mcp"
	"centrext/internal/workspace"
)

// Status is a check's outcome.
type Status int

const (
	OK Status = iota
	Warn
	Fail
)

func (s Status) String() string { return [...]string{"ok", "warn", "FAIL"}[s] }

// Check is one doctor finding. Fix, when set, is a command or step that fixes it.
type Check struct {
	Status  Status
	Area    string
	Message string
	Fix     string
}

// tokenPattern matches agent tokens: "cxt_" and 32 random bytes in base64url.
var tokenPattern = regexp.MustCompile(`cxt_[A-Za-z0-9_-]{43}`)

// Doctor checks connected clients, generated files and repos for leaked
// tokens (ADR-018). It changes nothing. cwd is the folder doctor runs in.
func Doctor(ctx context.Context, svc *application.Service, user domain.Actor, env Env, cwd string) []Check {
	var out []Check
	add := func(st Status, area, msg, fix string) { out = append(out, Check{st, area, msg, fix}) }

	st, err := LoadState(env.CentrextHome)
	if err != nil {
		add(Fail, "connections", "can't read "+filepath.Join(env.CentrextHome, stateFile)+": "+err.Error(), "")
		return out
	}
	if len(st.Agents) == 0 {
		add(Warn, "connections", "no clients are connected", "centrext connect claude-code (or cursor, claude-desktop)")
	}
	for _, client := range Clients {
		id, ok := st.Agents[client]
		if !ok {
			continue
		}
		agent, healthy := checkAgent(ctx, svc, user, env, client, id, add)
		switch client {
		case ClaudeCode:
			checkClaudeCode(env, add)
		case ClaudeDesktop:
			checkServerEntry(env, client, "centrext connect "+client, claudeDesktopConfig(env), "", add)
		case Cursor:
			checkCursor(ctx, svc, env, st, agent, healthy, add)
		}
	}

	// Tokens in files git tracks (ADR-018).
	scanned := map[string]bool{}
	dirs := sortedKeys(st.CursorProjects)
	if root := workspace.RepoRoot(cwd); root != "" {
		dirs = append(dirs, root)
	}
	for _, dir := range dirs {
		if scanned[dir] || workspace.RepoRoot(dir) != dir {
			continue
		}
		scanned[dir] = true
		scanRepo(ctx, svc, user, env, dir, add)
	}

	// This folder's marker, if any, must name a namespace that exists.
	if id, err := workspace.Resolve(cwd); err == nil && id.Kind == workspace.KindMarker {
		if ns, err := svc.FindNamespace(ctx, user, id.Value); err != nil {
			add(Fail, "this folder", fmt.Sprintf("%s in %s names namespace %s, which doesn't exist here", workspace.MarkerFile, id.Dir, id.Value),
				"centrext link NAMESPACE (in "+id.Dir+")")
		} else {
			add(OK, "this folder", "pinned to namespace "+ns.Name+" by "+filepath.Join(id.Dir, workspace.MarkerFile), "")
		}
	} else if err == nil && id.Ambiguous {
		add(Warn, "this folder", fmt.Sprintf("several git remotes (%v) and none is origin, so no namespace can be chosen", id.RemoteNames), "centrext link NAMESPACE")
	}

	if q, err := svc.ReviewQueue(ctx, user); err == nil {
		if n := len(q.Pending) + len(q.Unreviewed); n > 0 {
			add(OK, "review", fmt.Sprintf("%d item(s) await your review", n), "centrext review")
		}
	}
	return out
}

func checkAgent(ctx context.Context, svc *application.Service, user domain.Actor, env Env, client, id string, add func(Status, string, string, string)) (domain.Actor, bool) {
	fix := "centrext connect " + client
	ag, err := svc.GetAgent(ctx, user, id)
	if err != nil {
		add(Fail, client, "its agent "+id+" doesn't exist in this database", fix)
		return domain.Actor{}, false
	}
	if !ag.Active() {
		add(Fail, client, "its agent "+id+" is revoked", fix)
		return domain.Actor{}, false
	}
	tok, err := env.Tokens.Load(id)
	if err != nil {
		add(Fail, client, "no token for agent "+id+" in the keychain", fix)
		return domain.Actor{}, false
	}
	if owner, err := svc.AgentForToken(ctx, user, tok); err != nil || owner.ID != id {
		add(Fail, client, "the stored token doesn't belong to agent "+id, fix)
		return domain.Actor{}, false
	}
	seen := "never used yet"
	if ag.LastSeenAt != nil {
		seen = "last used " + ag.LastSeenAt.Local().Format(time.DateTime)
	}
	add(OK, client, "agent "+id+" is active, token in keychain, "+seen, "")
	actor, err := svc.ActAsAgent(ctx, user, id)
	return actor, err == nil
}

// checkCommand compares a configured command with this binary.
func checkCommand(area, fix, where, cmd string, env Env, add func(Status, string, string, string)) {
	if cmd == "" {
		add(Fail, area, where+" has no command", fix)
		return
	}
	if _, err := os.Stat(cmd); err != nil {
		add(Fail, area, where+" runs "+cmd+", which doesn't exist", fix)
	} else if cmd != env.Binary {
		add(Warn, area, where+" runs "+cmd+", not this binary ("+env.Binary+")", fix)
	}
}

// checkServerEntry checks mcpServers.centrext in a JSON config file.
func checkServerEntry(env Env, client, fix, path, wantArg string, add func(Status, string, string, string)) bool {
	if _, err := os.Stat(path); err != nil {
		add(Fail, client, path+" doesn't exist", fix)
		return false
	}
	m, err := readJSON(path)
	if err != nil {
		add(Fail, client, err.Error(), "")
		return false
	}
	servers, _ := m["mcpServers"].(map[string]any)
	entry, _ := servers[ServerName].(map[string]any)
	if entry == nil {
		add(Fail, client, "no "+ServerName+" MCP server in "+path, fix)
		return false
	}
	cmd, _ := entry["command"].(string)
	checkCommand(client, fix, path, cmd, env, add)
	if wantArg != "" && !strings.Contains(fmt.Sprint(entry["args"]), wantArg) {
		add(Warn, client, path+" doesn't pass "+wantArg, fix)
	}
	if strings.Contains(fmt.Sprint(entry), "cxt_") {
		add(Warn, client, path+" contains a token; Centrext only needs --client", fix)
	}
	return true
}

func checkClaudeCode(env Env, add func(Status, string, string, string)) {
	// Read-only: Claude Code owns ~/.claude.json (docs/clients.md).
	checkServerEntry(env, ClaudeCode, "centrext connect "+ClaudeCode, filepath.Join(env.UserHome, ".claude.json"), "", add)
	settings := filepath.Join(env.UserHome, ".claude", "settings.json")
	m, err := readJSON(settings)
	if err != nil {
		add(Fail, ClaudeCode, err.Error(), "")
		return
	}
	hooks, _ := m["hooks"].(map[string]any)
	ours := func(event string) []string {
		var out []string
		groups, _ := hooks[event].([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				if hm, ok := h.(map[string]any); ok && isOurHook(hm["command"], event) {
					out = append(out, hm["command"].(string))
				}
			}
		}
		return out
	}
	start, prompt := ours(SessionStartEvent), ours(PromptEvent)
	fix := "centrext connect " + ClaudeCode
	switch {
	case len(start) == 0:
		add(Fail, ClaudeCode, "no SessionStart hook in "+settings+", so sessions start without the summary", fix)
	case len(start) > 1:
		add(Warn, ClaudeCode, fmt.Sprintf("%d Centrext SessionStart hooks in %s; the summary is added more than once", len(start), settings), fix)
	case !strings.HasPrefix(start[0], shellQuote(env.Binary)+" "):
		add(Warn, ClaudeCode, "the SessionStart hook runs another centrext binary: "+start[0], fix)
	}
	switch {
	case len(prompt) == 0:
		add(OK, ClaudeCode, "per-prompt context is off", "")
	case len(prompt) > 1:
		add(Warn, ClaudeCode, fmt.Sprintf("%d Centrext UserPromptSubmit hooks in %s; context is added more than once", len(prompt), settings), fix+" --prompt-context on")
	case !strings.HasPrefix(prompt[0], shellQuote(env.Binary)+" "):
		add(Warn, ClaudeCode, "the UserPromptSubmit hook runs another centrext binary: "+prompt[0], fix+" --prompt-context on")
	default:
		add(OK, ClaudeCode, "per-prompt context is on (ADR-024)", "")
	}
}

func checkCursor(ctx context.Context, svc *application.Service, env Env, st State, agent domain.Actor, healthy bool, add func(Status, string, string, string)) {
	if len(st.CursorProjects) == 0 {
		add(Warn, Cursor, "connected, but no project is set up", "centrext connect cursor (inside a project)")
	}
	for _, dir := range sortedKeys(st.CursorProjects) {
		area := Cursor + " " + dir
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			add(Warn, area, "the project folder is gone", "centrext disconnect cursor --dir "+shellQuote(dir))
			continue
		}
		fix := "centrext connect cursor --dir " + shellQuote(dir)
		checkServerEntry(env, area, fix, filepath.Join(dir, ".cursor", "mcp.json"), "--workspace", add)
		rules := filepath.Join(dir, RulesFile)
		got, err := os.ReadFile(rules)
		if err != nil {
			add(Fail, area, "no rules file, so Cursor doesn't get the summary", fix)
		} else if healthy {
			if sum, err := svc.Summary(ctx, agent, st.CursorProjects[dir], 1000); err != nil {
				add(Fail, area, "can't build the summary: "+err.Error(), fix)
			} else if string(RenderRules(sum, mcpx.Guide)) != string(got) {
				add(Warn, area, "the rules file is out of date", fix)
			}
		}
		if _, err := env.run("git", "-C", dir, "ls-files", "--error-unmatch", "--", filepath.ToSlash(RulesFile)); err == nil {
			add(Warn, area, RulesFile+" is committed; it's generated per machine",
				"git -C "+shellQuote(dir)+" rm --cached "+filepath.ToSlash(RulesFile))
		}
	}
}

// scanRepo looks for agent tokens in files git tracks. It searches the
// working tree, not history: a token that was ever committed must be revoked.
func scanRepo(ctx context.Context, svc *application.Service, user domain.Actor, env Env, dir string, add func(Status, string, string, string)) {
	area := "tokens " + dir
	out, err := env.run("git", "-C", dir, "grep", "-l", "-I", "-E", tokenPattern.String())
	var exit *exec.ExitError
	switch {
	case errors.Is(err, exec.ErrNotFound):
		add(Warn, area, "git isn't installed, so tracked files weren't checked for tokens", "")
		return
	case errors.As(err, &exit) && exit.ExitCode() == 1:
		out = nil // no matches
	case err != nil:
		add(Warn, area, "couldn't search tracked files: "+strings.TrimSpace(string(out)), "")
		return
	}
	found := false
	for _, file := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if file == "" {
			continue
		}
		found = true
		b, _ := os.ReadFile(filepath.Join(dir, file))
		agents := map[string]bool{}
		for _, tok := range tokenPattern.FindAllString(string(b), -1) {
			if ag, err := svc.AgentForToken(ctx, user, tok); err == nil && ag.Active() {
				agents[ag.ID] = true
			}
		}
		if len(agents) == 0 {
			add(Warn, area, file+" (tracked by git) contains a Centrext token, not an active one", "remove it from the file")
			continue
		}
		for id := range agents {
			add(Fail, area, file+" (tracked by git) contains the active token of agent "+id,
				"centrext agent revoke "+id+", remove the token from the file, then centrext connect again")
		}
	}
	if !found {
		add(OK, area, "no tokens in files git tracks", "")
	}
}
