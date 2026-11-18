// Command centrext is the local Centrext CLI, plus the `mcp` server and
// `hook` modes that clients start (ADR-016).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"centrext/internal/application"
	"centrext/internal/connect"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/keystore"
	"centrext/internal/infrastructure/sqlite"
)

const version = "0.2.0-dev"

const usage = `centrext: the shared context layer for AI agents

Usage:
  centrext init [--name NAME]
  centrext namespace list
  centrext namespace create NAME [--parent NS] [--storage MODE]
  centrext namespace link NS IDENTITY
  centrext namespace policy NS MODE       local_only | local_primary | cloud_primary | shared
  centrext context list [--namespace NS] [--type TYPE] [--all]
  centrext context search QUERY... [--namespace NS] [--type TYPE] [--limit N]
  centrext context show ID
  centrext context history ID
  centrext context add --namespace NS --type TYPE --title TITLE --body BODY
  centrext context verify ID
  centrext context delete ID
  centrext review
  centrext review approve PROPOSAL_ID
  centrext review reject PROPOSAL_ID [--note NOTE]
  centrext agent register NAME --client CLIENT (--all | --namespace NS ...) [--print-token]
  centrext agent list
  centrext agent grant AGENT_ID (NS | --all)
  centrext agent revoke AGENT_ID [--reject-pending]
  centrext connect CLIENT [--dir DIR] [--namespace NS ...]   CLIENT: claude-code, cursor, claude-desktop
                   [--prompt-context on|off]               claude-code: add matching context to each prompt
  centrext disconnect CLIENT [--dir DIR]
  centrext export [--dir DIR]              write user-verified decisions to .centrext-export/ in this repo
  centrext embeddings enable [--provider openai|voyage|openai-compatible] [--model M] [--base-url URL]
  centrext embeddings key | status | sync | disable    semantic search through a hosted API
  centrext stats                           capture metrics: what gets saved, retrieved and used
  centrext store status | reencrypt         show or upgrade the store's encryption (ADR-025)
  centrext doctor                          check the install, clients and repos for leaked tokens
  centrext link NAMESPACE [--dir DIR]      pin this repo with a .centrext marker
  centrext mcp (--client CLIENT | --agent AGENT_ID) [--workspace DIR]
  centrext hook (session-start | prompt) (--client CLIENT | --agent AGENT_ID)
  centrext version

Environment:
  CENTREXT_HOME     data directory (default ~/.centrext)
  CENTREXT_DB_KEY   64 hex digits; overrides the OS keychain (temporary workaround)
  CENTREXT_TOKEN    agent token for mcp/hook; overrides the keychain
  CENTREXT_EMBEDDINGS_KEY (or OPENAI_API_KEY, VOYAGE_API_KEY)  embeddings API key; overrides the keychain
`

func main() {
	if err := run(context.Background(), os.Args[1:]); errors.Is(err, errDoctorFailed) {
		os.Exit(1)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, "error:", explain(err))
		os.Exit(1)
	}
}

func explain(err error) string {
	switch {
	case errors.Is(err, domain.ErrStaleVersion):
		return "the context changed since you read it; run the command again"
	case errors.Is(err, domain.ErrNotFound):
		return "not found"
	}
	return err.Error()
}

func home() (string, error) {
	if h := os.Getenv("CENTREXT_HOME"); h != "" {
		return filepath.Abs(h)
	}
	d, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, ".centrext"), nil
}

// app is an opened store plus the local user.
type app struct {
	svc   *application.Service
	store *sqlite.Store
	user  domain.Actor
}

func openApp(ctx context.Context, create bool) (*app, error) {
	h, err := home()
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(h, "centrext.db")
	if !create {
		if _, err := os.Stat(dbPath); err != nil {
			return nil, fmt.Errorf("no database at %s; run `centrext init`", dbPath)
		}
	}
	key, _, err := keystore.Load(h, sqlite.KeySize, create)
	if err != nil {
		return nil, err
	}
	store, err := sqlite.Open(ctx, dbPath, key)
	if err != nil {
		return nil, err
	}
	var svc *application.Service
	opts := []application.Option{application.WithWarn(func(msg string) { fmt.Fprintln(os.Stderr, "centrext:", msg) })}
	if emb := loadEmbedder(h); emb != nil {
		opts = append(opts, application.WithEmbedder(emb))
	}
	svc = application.New(store, append(opts, application.WithOnChange(func(ctx context.Context, namespaceID string) {
		// Keep Cursor rules files current (ADR-014). Never fails the change.
		if err := connect.RefreshCursorRules(ctx, svc, h, namespaceID); err != nil {
			fmt.Fprintln(os.Stderr, "centrext: couldn't refresh Cursor rules:", err)
		}
	}))...)
	return &app{svc: svc, store: store}, nil
}

func (a *app) close() { a.store.Close() }

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	case "version":
		fmt.Println("centrext", version)
		return nil
	case "init":
		return cmdInit(ctx, args[1:])
	case "mcp":
		return cmdMCP(ctx, args[1:])
	case "hook":
		return cmdHook(ctx, args[1:])
	case "doctor":
		return cmdDoctor(ctx)
	case "store":
		return cmdStore(ctx, args[1:]) // before openApp: reencrypt needs the only connection
	}

	a, err := openApp(ctx, false)
	if err != nil {
		return err
	}
	defer a.close()
	if a.user, err = a.svc.LocalUser(ctx); err != nil {
		return err
	}
	switch args[0] {
	case "namespace", "ns":
		return a.cmdNamespace(ctx, args[1:])
	case "context", "ctx":
		return a.cmdContext(ctx, args[1:])
	case "review":
		return a.cmdReview(ctx, args[1:])
	case "agent":
		return a.cmdAgent(ctx, args[1:])
	case "link":
		return a.cmdLink(ctx, args[1:])
	case "stats":
		return a.cmdStats(ctx)
	case "export":
		return a.cmdExport(ctx, args[1:])
	case "embeddings":
		h, err := home()
		if err != nil {
			return err
		}
		return a.cmdEmbeddings(ctx, h, args[1:])
	case "connect":
		return a.cmdConnect(ctx, args[1:], false)
	case "disconnect":
		return a.cmdConnect(ctx, args[1:], true)
	}
	return fmt.Errorf("unknown command %q; run `centrext help`", args[0])
}

func cmdInit(ctx context.Context, args []string) error {
	fs := newFlags("init")
	defaultName := "me"
	if u, err := user.Current(); err == nil && u.Username != "" {
		defaultName = u.Username
	}
	name := fs.String("name", defaultName, "your name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	h, err := home()
	if err != nil {
		return err
	}
	key, src, err := keystore.Load(h, sqlite.KeySize, true)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(h, "centrext.db")
	store, err := sqlite.Open(ctx, dbPath, key)
	if err != nil {
		return err
	}
	defer store.Close()
	u, err := application.New(store).Init(ctx, *name)
	if err != nil {
		return err
	}
	fmt.Printf("Initialized %s\n  user: %s (%s)\n  key:  %s\n  namespace: %s (local_only)\n", dbPath, u.Name, u.ID, src, application.PersonalNamespace)
	return nil
}
