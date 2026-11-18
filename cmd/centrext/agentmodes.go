package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"centrext/internal/connect"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/keystore"
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
	if len(args) == 0 || args[0] != "session-start" {
		fmt.Fprintln(os.Stderr, "centrext hook: unknown hook; supported: session-start")
		return nil
	}
	fs := newFlags("hook session-start")
	agentID := fs.String("agent", "", "agent ID")
	client := fs.String("client", "claude-code", "claude-code or cursor")
	if _, err := parse(fs, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "centrext hook:", err)
		return nil
	}
	out, err := sessionSummary(ctx, *agentID, *client, os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "centrext hook:", err)
		return nil
	}
	if *client == "cursor" {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"additional_context": out})
	}
	fmt.Print(out)
	return nil
}

func sessionSummary(ctx context.Context, agentID, client string, stdin io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var in struct {
		Cwd            string   `json:"cwd"`
		WorkspaceRoots []string `json:"workspace_roots"` // Cursor
	}
	b, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	_ = json.Unmarshal(b, &in)
	dir := in.Cwd
	if len(in.WorkspaceRoots) > 0 {
		dir = in.WorkspaceRoots[0]
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	a, actor, err := agentApp(ctx, agentID, client)
	if err != nil {
		return "", err
	}
	defer a.close()
	id, err := workspace.Resolve(dir)
	if err != nil {
		return "", err
	}
	r, err := a.svc.ResolveNamespace(ctx, actor, id)
	if err != nil {
		return "", err
	}
	sum, err := a.svc.Summary(ctx, actor, r.Namespace.ID, 1000)
	if err != nil {
		return "", err
	}
	if r.Note != "" {
		sum += "\nNote: " + r.Note + "\n"
	}
	return sum, nil
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
