package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/sqlite"
	mcpx "centrext/internal/transport/mcp"
)

type env struct {
	t    *testing.T
	ctx  context.Context
	svc  *application.Service
	user domain.Actor
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	s, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "c.db"), bytes.Repeat([]byte{7}, sqlite.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	svc := application.New(s)
	if _, err := svc.Init(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	u, err := svc.LocalUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ctx: ctx, svc: svc, user: u}
}

func (e *env) agent(name string, nsIDs ...string) domain.Actor {
	e.t.Helper()
	_, token, err := e.svc.RegisterAgent(e.ctx, e.user, application.RegisterAgentInput{Name: name, Client: name,
		NamespaceIDs: nsIDs, AllNamespaces: len(nsIDs) == 0})
	if err != nil {
		e.t.Fatal(err)
	}
	a, err := e.svc.Authenticate(e.ctx, token)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func repo(t *testing.T, remote string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shop")
	os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+remote+"\n"), 0o644)
	return dir
}

// connect starts a server for the agent and returns a connected client.
// roots, if set, is advertised by the client (like Claude Code does).
func (e *env) connect(actor domain.Actor, opts mcpx.Options, roots ...string) *sdk.ClientSession {
	e.t.Helper()
	if opts.Workspace == "" && len(roots) == 0 {
		opts.Workspace = e.t.TempDir() // not a repo: Personal namespace
	}
	srv, _ := mcpx.New(e.ctx, e.svc, actor, opts)
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(e.ctx, st, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { ss.Close() })
	var sopts *sdk.ClientSessionOptions
	if len(roots) > 0 {
		// Roots work only before protocol 2026-07-28 (SEP-2577), which is what
		// clients that send roots speak today.
		sopts = &sdk.ClientSessionOptions{ProtocolVersion: "2025-11-25"}
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	for _, r := range roots {
		client.AddRoots(&sdk.Root{URI: "file://" + r})
	}
	cs, err := client.Connect(e.ctx, ct, sopts)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and decodes its structured result. wantErr says whether
// the call should fail; the error text is returned.
func call[T any](t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, wantErr bool) (T, string) {
	t.Helper()
	var out T
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", tool, err)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			text += tc.Text
		}
	}
	if res.IsError != wantErr {
		t.Fatalf("%s: IsError=%v, want %v: %s", tool, res.IsError, wantErr, text)
	}
	if !wantErr {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: decode: %v", tool, err)
		}
	}
	return out, text
}

// The core scenario over MCP: Claude Code (folder from roots) saves a
// decision; Cursor (folder from --workspace, another clone URL) gets it in the
// server instructions and finds it with context_search.
func TestCoreScenarioOverMCP(t *testing.T) {
	e := newEnv(t)
	claude := e.connect(e.agent("claude-code"), mcpx.Options{}, repo(t, "git@github.com:acme/shop.git"))

	st, _ := call[mcpx.StatusOut](t, claude, "context_status", nil, false)
	if st.ResolvedFrom != "git_remote" || st.Identity != "git:github.com/acme/shop" {
		t.Fatalf("status: %+v", st)
	}

	saved, _ := call[mcpx.WriteOut](t, claude, "context_save", map[string]any{
		"type": "architecture", "title": "Orders database", "origin": "explicit",
		"body": "Orders live in PostgreSQL behind a transactional outbox. Never last-write-wins."}, false)
	if saved.Status != "accepted" || saved.Context == nil || saved.Context.Trust != "session_confirmed" {
		t.Fatalf("save: %+v", saved)
	}

	cursor := e.connect(e.agent("cursor"), mcpx.Options{Workspace: repo(t, "https://github.com/Acme/Shop")})
	if ins := cursor.InitializeResult().Instructions; !strings.Contains(ins, "Orders database") || !strings.Contains(ins, "context_search") {
		t.Fatalf("instructions should carry the summary:\n%s", ins)
	}
	found, _ := call[mcpx.SearchOut](t, cursor, "context_search", map[string]any{"query": "which database do orders use"}, false)
	if len(found.Results) == 0 || found.Results[0].Title != "Orders database" {
		t.Fatalf("search: %+v", found)
	}
	if r := found.Results[0]; r.Trust != "session_confirmed" || !strings.HasPrefix(r.Author, "agent:") || r.Version != 1 {
		t.Fatalf("provenance missing: %+v", r)
	}

	// Revising a decision/architecture item from an agent waits for the user.
	upd, _ := call[mcpx.WriteOut](t, cursor, "context_update", map[string]any{"id": found.Results[0].ID, "base_version": 1,
		"title": "Orders database", "body": "Orders move to DynamoDB.", "origin": "explicit"}, false)
	if upd.Status != "pending" {
		t.Fatalf("update should wait for approval: %+v", upd)
	}
	_, msg := call[mcpx.WriteOut](t, cursor, "context_update", map[string]any{"id": found.Results[0].ID, "base_version": 9,
		"title": "x", "body": "y", "origin": "explicit"}, true)
	if !strings.Contains(msg, "base_version") {
		t.Fatalf("stale version message: %s", msg)
	}
}

func TestGovernanceOverMCP(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.agent("claude-desktop"), mcpx.Options{})

	// Agents can't claim the user's origin or save silently through context_save.
	for _, origin := range []string{"user", "silent", ""} {
		call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "fact", "title": "t", "body": "b", "origin": origin}, true)
	}
	// Secrets are refused.
	_, msg := call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "fact", "title": "AWS",
		"body": "key AKIAIOSFODNN7EXAMPLE", "origin": "explicit"}, true)
	if !strings.Contains(msg, "secret") {
		t.Fatalf("secret message: %s", msg)
	}
	// Suggested saves are capped per session.
	for i := 0; i < mcpx.DefaultSuggestionLimit; i++ {
		call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "preference", "title": "Pref " + string(rune('A'+i)),
			"body": "Prefers short answers.", "origin": "suggested"}, false)
	}
	call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "preference", "title": "One more",
		"body": "x", "origin": "suggested"}, true)
	// Explicit saves still work after the cap.
	call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "preference", "title": "Tone",
		"body": "Plain language.", "origin": "explicit"}, false)

	// Silent observations: agent_reported, expire.
	obs, _ := call[mcpx.WriteOut](t, cs, "context_propose", map[string]any{"kind": "observation",
		"title": "Trip", "body": "User is planning a trip to Japan in April."}, false)
	if obs.Context == nil || obs.Context.Trust != "agent_reported" || obs.Context.ExpiresAt == "" {
		t.Fatalf("observation: %+v", obs)
	}
	// Topic namespaces wait for approval.
	ns, _ := call[mcpx.WriteOut](t, cs, "context_propose", map[string]any{"kind": "namespace",
		"title": "Trip planning", "body": "Keep trip details together."}, false)
	if ns.Status != "pending" {
		t.Fatalf("namespace proposal: %+v", ns)
	}
	// Idempotent retries replay.
	args := map[string]any{"type": "fact", "title": "Home", "body": "Lives in Pune.", "origin": "explicit", "idempotency_key": "k1"}
	first, _ := call[mcpx.WriteOut](t, cs, "context_save", args, false)
	again, _ := call[mcpx.WriteOut](t, cs, "context_save", args, false)
	if again.Status != "replayed" || again.ProposalID != first.ProposalID {
		t.Fatalf("replay: %+v vs %+v", again, first)
	}
}

func TestAccessOverMCP(t *testing.T) {
	e := newEnv(t)
	work, err := e.svc.CreateNamespace(e.ctx, e.user, application.NewNamespace{Name: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	in := application.ProposeInput{NamespaceID: work.ID, Kind: domain.KindCreate, Origin: domain.OriginUser,
		Type: domain.TypeDecision, Title: "Kafka is banned", Body: "No Kafka without a demonstrated requirement."}
	if _, err := e.svc.Propose(e.ctx, e.user, in); err != nil {
		t.Fatal(err)
	}
	personal, _ := e.svc.FindNamespace(e.ctx, e.user, application.PersonalNamespace)
	cs := e.connect(e.agent("limited", personal.ID), mcpx.Options{})

	if res, _ := call[mcpx.SearchOut](t, cs, "context_search", map[string]any{"query": "kafka"}, false); len(res.Results) != 0 {
		t.Fatalf("leaked: %+v", res)
	}
	call[mcpx.SearchOut](t, cs, "context_search", map[string]any{"query": "kafka", "namespace": "Work"}, true)
	call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "fact", "title": "t", "body": "b",
		"origin": "explicit", "namespace": work.ID}, true)

	// The namespaces resource lists only what the agent can see.
	rr, err := cs.ReadResource(e.ctx, &sdk.ReadResourceParams{URI: "centrext://namespaces"})
	if err != nil {
		t.Fatal(err)
	}
	if txt := rr.Contents[0].Text; strings.Contains(txt, "Work") || !strings.Contains(txt, "Personal") {
		t.Fatalf("namespaces resource: %s", txt)
	}
	// Overview of a namespace it can't access fails.
	if _, err := cs.ReadResource(e.ctx, &sdk.ReadResourceParams{URI: "centrext://namespace/" + work.ID + "/overview"}); err == nil {
		t.Fatal("overview of a forbidden namespace should fail")
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	e := newEnv(t)
	cs := e.connect(e.agent("claude-desktop"), mcpx.Options{})
	saved, _ := call[mcpx.WriteOut](t, cs, "context_save", map[string]any{"type": "convention", "title": "Units",
		"body": "Use metric units.", "origin": "explicit"}, false)

	nsID := saved.Context.Namespace
	for uri, want := range map[string]string{
		"centrext://namespace/" + nsID + "/overview":  "Units",
		"centrext://namespace/" + nsID + "/decisions": "metric",
		"centrext://context/" + saved.Context.ID:      "metric",
	} {
		rr, err := cs.ReadResource(e.ctx, &sdk.ReadResourceParams{URI: uri})
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if !strings.Contains(rr.Contents[0].Text, want) {
			t.Fatalf("%s: %s", uri, rr.Contents[0].Text)
		}
	}
	p, err := cs.GetPrompt(e.ctx, &sdk.GetPromptParams{Name: "project-onboarding"})
	if err != nil {
		t.Fatal(err)
	}
	if txt := p.Messages[0].Content.(*sdk.TextContent).Text; !strings.Contains(txt, "Units") || !strings.Contains(txt, "context_save") {
		t.Fatalf("onboarding prompt: %s", txt)
	}
}
