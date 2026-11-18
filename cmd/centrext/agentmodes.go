package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"centrext/internal/application"
	"centrext/internal/connect"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/keystore"
	"centrext/internal/session"
	mcpx "centrext/internal/transport/mcp"
	"centrext/internal/workspace"
)

// agentApp opens the store and authenticates an agent: token from
// CENTREXT_TOKEN, else from the keychain by agent ID (ADR-018). With no agent
// ID, the agent connected for client is used (`centrext connect`).
func agentApp(ctx context.Context, agentID, client string) (*app, domain.Actor, error) {
	h, err := home()
	if err != nil {
		return nil, domain.Actor{}, err
	}
	if agentID == "" && client != "" && os.Getenv(keystore.EnvToken) == "" {
		if agentID, err = connect.AgentFor(h, client); err != nil {
			return nil, domain.Actor{}, err
		}
	}
	token, err := keystore.LoadAgentToken(h, agentID)
	if err != nil {
		return nil, domain.Actor{}, err
	}
	a, err := openApp(ctx, false)
	if err != nil {
		return nil, domain.Actor{}, err
	}
	actor, err := a.svc.Authenticate(ctx, token)
	if err == nil && agentID != "" && actor.ID != agentID {
		err = fmt.Errorf("token belongs to agent %s, not %s", actor.ID, agentID)
	}
	if err != nil {
		a.close()
		if errors.Is(err, domain.ErrUnauthenticated) {
			err = errors.New("invalid or revoked agent token; run `centrext connect`")
		}
		return nil, domain.Actor{}, err
	}
	return a, actor, nil
}

// cmdMCP runs the MCP server over stdio for one agent session (ADR-016).
func cmdMCP(ctx context.Context, args []string) error {
	fs := newFlags("mcp")
	agentID := fs.String("agent", "", "agent ID (its token is read from the keychain)")
	client := fs.String("client", "", "use the agent connected for this client")
	ws := fs.String("workspace", "", "project folder (e.g. Cursor's ${workspaceFolder})")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	if *ws == "${workspaceFolder}" { // the client didn't expand it
		*ws = ""
	}
	a, actor, err := agentApp(ctx, *agentID, *client)
	if err != nil {
		return err
	}
	defer a.close()
	srv, _ := mcpx.New(ctx, a.svc, actor, mcpx.Options{Version: version, Workspace: *ws})
	return srv.Run(ctx, &sdk.StdioTransport{})
}

// cmdHook handles client hooks. A hook must never break the user's session:
// problems go to stderr and the exit code is always 0.
func cmdHook(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "session-start" && args[0] != "prompt") {
		fmt.Fprintln(os.Stderr, "centrext hook: unknown hook; supported: session-start, prompt")
		return nil
	}
	fs := newFlags("hook " + args[0])
	agentID := fs.String("agent", "", "agent ID")
	client := fs.String("client", "claude-code", "claude-code or cursor")
	if _, err := parse(fs, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "centrext hook:", err)
		return nil
	}
	in := readHookInput(os.Stdin)
	var out string
	var err error
	if args[0] == "prompt" {
		out, err = promptContext(ctx, *agentID, *client, in)
	} else {
		out, err = sessionSummary(ctx, *agentID, *client, in)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "centrext hook:", err)
		return nil
	}
	if out == "" {
		return nil
	}
	if *client == "cursor" {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"additional_context": out})
	}
	fmt.Print(out)
	return nil
}

// hookInput is the JSON a client sends on stdin. Only the fields used here.
type hookInput struct {
	SessionID      string   `json:"session_id"`
	Cwd            string   `json:"cwd"`
	WorkspaceRoots []string `json:"workspace_roots"` // Cursor
	Prompt         string   `json:"prompt"`          // UserPromptSubmit
}

func readHookInput(r io.Reader) hookInput {
	var in hookInput
	b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
	_ = json.Unmarshal(b, &in)
	return in
}

func (in hookInput) dir() string {
	if len(in.WorkspaceRoots) > 0 {
		return in.WorkspaceRoots[0]
	}
	if in.Cwd != "" {
		return in.Cwd
	}
	d, _ := os.Getwd()
	return d
}

// hookNamespace opens the store as the agent and resolves the session's namespace.
func hookNamespace(ctx context.Context, agentID, client string, in hookInput) (*app, domain.Actor, application.Resolved, error) {
	a, actor, err := agentApp(ctx, agentID, client)
	if err != nil {
		return nil, actor, application.Resolved{}, err
	}
	id, err := workspace.Resolve(in.dir())
	if err == nil {
		var r application.Resolved
		if r, err = a.svc.ResolveNamespace(ctx, actor, id); err == nil {
			return a, actor, r, nil
		}
	}
	a.close()
	return nil, actor, application.Resolved{}, err
}

func sessionSummary(ctx context.Context, agentID, client string, in hookInput) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	a, actor, r, err := hookNamespace(ctx, agentID, client, in)
	if err != nil {
		return "", err
	}
	defer a.close()
	sum, ids, err := a.svc.SummaryWithIDs(ctx, actor, r.Namespace.ID, 1000)
	if err != nil {
		return "", err
	}
	if r.Note != "" {
		sum += "\nNote: " + r.Note + "\n"
	}
	if h, err := home(); err == nil && in.SessionID != "" {
		d := session.Load(h, in.SessionID)
		d.Add(ids...)
		if err := d.Save(time.Now()); err != nil {
			fmt.Fprintln(os.Stderr, "centrext hook: couldn't record the session:", err)
		}
	}
	return sum, nil
}

// promptBodyChars caps an injected item's body.
const promptBodyChars = 600

// promptContext returns at most one saved item clearly relevant to the
// prompt, skipping what this session already got (ADR-024). The prompt is
// matched locally and never stored.
func promptContext(ctx context.Context, agentID, client string, in hookInput) (string, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	a, actor, r, err := hookNamespace(ctx, agentID, client, in)
	if err != nil {
		return "", err
	}
	defer a.close()
	h, err := home()
	if err != nil {
		return "", err
	}
	d := session.Load(h, in.SessionID)
	res, err := a.svc.PromptContext(ctx, actor, application.PromptQuery{Prompt: in.Prompt, NamespaceID: r.Namespace.ID, Exclude: d.IDs})
	if err != nil || len(res.Items) == 0 {
		return "", err
	}
	var b strings.Builder
	b.WriteString("Centrext: saved context that may apply to this request. It was saved earlier, possibly in another AI tool; prefer it over assumptions.\n")
	for _, it := range res.Items {
		body := strings.Join(strings.Fields(it.Head.Body), " ")
		if utf8.RuneCountInString(body) > promptBodyChars {
			body = string([]rune(body)[:promptBodyChars]) + "…"
		}
		fmt.Fprintf(&b, "- [%s, %s] %s (%s): %s\n", it.Context.Type, it.Context.HeadTrust, it.Head.Title, it.Context.ID, body)
		d.Add(it.Context.ID)
	}
	b.WriteString("If it shaped your answer, call context_used with its id. Use context_search for more.\n")
	if err := d.Save(time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "centrext hook: couldn't record the session:", err)
	}
	return b.String(), nil
}

// cmdLink pins the current repo (or --dir) to a namespace with a .centrext marker.
func (a *app) cmdLink(ctx context.Context, args []string) error {
	fs := newFlags("link")
	dir := fs.String("dir", "", "folder to pin (default: the repo root, or the current folder)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "NAMESPACE"); err != nil {
		return err
	}
	ns, err := a.svc.FindNamespace(ctx, a.user, pos[0])
	if err != nil {
		return err
	}
	target := *dir
	if target == "" {
		cwd, _ := os.Getwd()
		target = cwd
		if root := workspace.RepoRoot(cwd); root != "" {
			target = root
		}
	}
	if err := workspace.WriteMarker(target, ns.ID); err != nil {
		return err
	}
	fmt.Printf("Pinned %s to namespace %s. Commit %s so every clone uses it.\n", target, ns.Name, workspace.MarkerFile)
	return nil
}
