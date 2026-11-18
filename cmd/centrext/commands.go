package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/export"
	"centrext/internal/infrastructure/keystore"
	"centrext/internal/workspace"
)

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse lets flags appear after positional arguments ("revoke ID --reject-pending").
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, fmt.Errorf("%s: %w", fs.Name(), err)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func table() *tabwriter.Writer { return tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0) }

func ts(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func need(pos []string, n int, what string) error {
	if len(pos) < n {
		return fmt.Errorf("missing %s; run `centrext help`", what)
	}
	return nil
}

// ---- namespace

func (a *app) cmdNamespace(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		ns, err := a.svc.ListNamespaces(ctx, a.user)
		if err != nil {
			return err
		}
		w := table()
		fmt.Fprintln(w, "ID\tNAME\tPARENT\tSTORAGE")
		for _, n := range ns {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.ID, n.Name, n.ParentID, n.StorageMode)
		}
		return w.Flush()
	case "create":
		fs := newFlags("namespace create")
		parent := fs.String("parent", "", "parent namespace id or name")
		storage := fs.String("storage", string(domain.StorageLocalOnly), "storage mode")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if err := need(pos, 1, "NAME"); err != nil {
			return err
		}
		parentID := ""
		if *parent != "" {
			p, err := a.svc.FindNamespace(ctx, a.user, *parent)
			if err != nil {
				return fmt.Errorf("parent: %w", err)
			}
			parentID = p.ID
		}
		n, err := a.svc.CreateNamespace(ctx, a.user, application.NewNamespace{Name: pos[0], ParentID: parentID, StorageMode: domain.StorageMode(*storage)})
		if err != nil {
			return err
		}
		fmt.Printf("Created namespace %s (%s, %s)\n", n.Name, n.ID, n.StorageMode)
		return nil
	case "policy":
		if err := need(args[1:], 2, "NS and MODE (local_only, local_primary, cloud_primary, shared)"); err != nil {
			return err
		}
		n, err := a.svc.FindNamespace(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		n, err = a.svc.SetStorageMode(ctx, a.user, n.ID, domain.StorageMode(args[2]))
		if err != nil {
			return err
		}
		fmt.Printf("Namespace %s is now %s\n", n.Name, n.StorageMode)
		return nil
	case "link":
		if err := need(args[1:], 2, "NS and IDENTITY"); err != nil {
			return err
		}
		n, err := a.svc.FindNamespace(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		if err := a.svc.AddNamespaceAlias(ctx, a.user, n.ID, args[2]); err != nil {
			return err
		}
		fmt.Printf("Linked %s to %s\n", args[2], n.Name)
		return nil
	}
	return fmt.Errorf("unknown namespace command %q", args[0])
}

// ---- context

func (a *app) cmdContext(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		fs := newFlags("context list")
		nsFlag := fs.String("namespace", "", "namespace id or name")
		typ := fs.String("type", "", "context type")
		all := fs.Bool("all", false, "include superseded, deleted and expired")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		q := application.ListQuery{IncludeInactive: *all}
		if *nsFlag != "" {
			n, err := a.svc.FindNamespace(ctx, a.user, *nsFlag)
			if err != nil {
				return err
			}
			q.NamespaceID = n.ID
		}
		if *typ != "" {
			q.Types = []domain.ContextType{domain.ContextType(*typ)}
		}
		views, err := a.svc.ListContexts(ctx, a.user, q)
		if err != nil {
			return err
		}
		w := table()
		fmt.Fprintln(w, "ID\tTYPE\tTRUST\tV\tSTATUS\tTITLE")
		for _, v := range views {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", v.Context.ID, v.Context.Type, v.Context.HeadTrust,
				v.Context.CurrentVersion, v.Context.Status, v.Head.Title)
		}
		return w.Flush()
	case "search":
		fs := newFlags("context search")
		nsFlag := fs.String("namespace", "", "namespace id or name")
		typ := fs.String("type", "", "context type")
		limit := fs.Int("limit", 5, "maximum results")
		words, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		q := application.SearchQuery{Query: strings.Join(words, " "), Limit: *limit}
		if *nsFlag != "" {
			n, err := a.svc.FindNamespace(ctx, a.user, *nsFlag)
			if err != nil {
				return err
			}
			q.NamespaceID = n.ID
		}
		if *typ != "" {
			q.Types = []domain.ContextType{domain.ContextType(*typ)}
		}
		res, err := a.svc.Search(ctx, a.user, q)
		if err != nil {
			return err
		}
		w := table()
		fmt.Fprintln(w, "SCORE\tID\tTYPE\tTRUST\tTITLE")
		for _, it := range res.Items {
			fmt.Fprintf(w, "%.2f\t%s\t%s\t%s\t%s\n", it.Score, it.Context.ID, it.Context.Type, it.Context.HeadTrust, it.Head.Title)
		}
		return w.Flush()
	case "show":
		if err := need(args[1:], 1, "ID"); err != nil {
			return err
		}
		v, err := a.svc.GetContext(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		c := v.Context
		fmt.Printf("%s\n\n%s\n\n", v.Head.Title, v.Head.Body)
		fmt.Printf("id:        %s\ntype:      %s\ntrust:     %s\nversion:   %d\nstatus:    %s\nnamespace: %s\nauthor:    %s %s\nupdated:   %s\n",
			c.ID, c.Type, c.HeadTrust, c.CurrentVersion, c.Status, c.NamespaceID, v.Head.AuthorKind, v.Head.AuthorID, ts(c.UpdatedAt))
		if c.ExpiresAt != nil {
			fmt.Printf("expires:   %s\n", ts(*c.ExpiresAt))
		}
		if c.SupersededBy != "" {
			fmt.Printf("superseded by: %s\n", c.SupersededBy)
		}
		return nil
	case "history":
		if err := need(args[1:], 1, "ID"); err != nil {
			return err
		}
		revs, err := a.svc.History(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		w := table()
		fmt.Fprintln(w, "V\tTRUST\tAUTHOR\tWHEN\tTITLE")
		for _, r := range revs {
			fmt.Fprintf(w, "%d\t%s\t%s %s\t%s\t%s\n", r.Version, r.Trust, r.AuthorKind, r.AuthorID, ts(r.CreatedAt), r.Title)
		}
		return w.Flush()
	case "add":
		fs := newFlags("context add")
		nsFlag := fs.String("namespace", "", "namespace id or name")
		typ := fs.String("type", "", "context type")
		title := fs.String("title", "", "title")
		body := fs.String("body", "", "body")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		if *nsFlag == "" {
			*nsFlag = application.PersonalNamespace
		}
		n, err := a.svc.FindNamespace(ctx, a.user, *nsFlag)
		if err != nil {
			return err
		}
		res, err := a.svc.Propose(ctx, a.user, application.ProposeInput{NamespaceID: n.ID, Kind: domain.KindCreate,
			Origin: domain.OriginUser, Type: domain.ContextType(*typ), Title: *title, Body: *body})
		if err != nil {
			return err
		}
		fmt.Printf("Saved %s (%s, user_verified)\n", res.Context.Context.ID, res.Context.Context.Type)
		return nil
	case "verify", "delete":
		if err := need(args[1:], 1, "ID"); err != nil {
			return err
		}
		v, err := a.svc.GetContext(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		if args[0] == "verify" {
			if v.Context.Reviewed {
				fmt.Printf("%s is already user_verified (v%d)\n", v.Context.ID, v.Context.CurrentVersion)
				return nil
			}
			v, err = a.svc.VerifyContext(ctx, a.user, v.Context.ID, v.Context.CurrentVersion)
			if err != nil {
				return err
			}
			fmt.Printf("Verified %s (now v%d, user_verified)\n", v.Context.ID, v.Context.CurrentVersion)
			return nil
		}
		if err := a.svc.DeleteContext(ctx, a.user, v.Context.ID, v.Context.CurrentVersion); err != nil {
			return err
		}
		fmt.Printf("Deleted %s (kept as a tombstone)\n", v.Context.ID)
		return nil
	}
	return fmt.Errorf("unknown context command %q", args[0])
}

// ---- review

func (a *app) cmdReview(ctx context.Context, args []string) error {
	if len(args) == 0 {
		r, err := a.svc.ReviewQueue(ctx, a.user)
		if err != nil {
			return err
		}
		if len(r.Pending) == 0 && len(r.Unreviewed) == 0 {
			fmt.Println("Nothing to review.")
			return nil
		}
		if len(r.Pending) > 0 {
			fmt.Printf("Proposals waiting for approval (%d):\n", len(r.Pending))
			w := table()
			fmt.Fprintln(w, "  ID\tKIND\tAGENT\tTARGET\tTITLE")
			for _, p := range r.Pending {
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", p.ID, p.Kind, p.AgentID, p.TargetContextID, p.Title)
			}
			w.Flush()
			fmt.Println("  approve: centrext review approve ID   reject: centrext review reject ID")
		}
		if len(r.Unreviewed) > 0 {
			fmt.Printf("\nSaved by agents, not yet verified (%d):\n", len(r.Unreviewed))
			w := table()
			fmt.Fprintln(w, "  ID\tTYPE\tTRUST\tTITLE")
			for _, v := range r.Unreviewed {
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", v.Context.ID, v.Context.Type, v.Context.HeadTrust, v.Head.Title)
			}
			w.Flush()
			fmt.Println("  verify: centrext context verify ID    delete: centrext context delete ID")
		}
		return nil
	}
	switch args[0] {
	case "approve":
		if err := need(args[1:], 1, "PROPOSAL_ID"); err != nil {
			return err
		}
		res, err := a.svc.ApproveProposal(ctx, a.user, args[1])
		if err != nil {
			return err
		}
		fmt.Printf("Approved %s -> %s\n", res.Proposal.ID, res.Proposal.ResultContextID)
		return nil
	case "reject":
		fs := newFlags("review reject")
		note := fs.String("note", "", "reason")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if err := need(pos, 1, "PROPOSAL_ID"); err != nil {
			return err
		}
		if err := a.svc.RejectProposal(ctx, a.user, pos[0], *note); err != nil {
			return err
		}
		fmt.Printf("Rejected %s\n", pos[0])
		return nil
	}
	return fmt.Errorf("unknown review command %q", args[0])
}

// ---- agent

func (a *app) cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "register":
		fs := newFlags("agent register")
		client := fs.String("client", "", "client, e.g. claude-code, cursor, claude-desktop")
		all := fs.Bool("all", false, "grant all namespaces")
		printToken := fs.Bool("print-token", false, "print the token instead of storing it in the keychain")
		var nss multi
		fs.Var(&nss, "namespace", "grant a namespace (repeatable)")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if err := need(pos, 1, "NAME"); err != nil {
			return err
		}
		var ids []string
		for _, n := range nss {
			ns, err := a.svc.FindNamespace(ctx, a.user, n)
			if err != nil {
				return fmt.Errorf("namespace %s: %w", n, err)
			}
			ids = append(ids, ns.ID)
		}
		ag, token, err := a.svc.RegisterAgent(ctx, a.user, application.RegisterAgentInput{Name: pos[0], Client: *client, NamespaceIDs: ids, AllNamespaces: *all})
		if err != nil {
			return err
		}
		fmt.Printf("Registered agent %s (%s)\n", ag.Name, ag.ID)
		if h, err := home(); err == nil && !*printToken {
			if src, err := keystore.StoreAgentToken(h, ag.ID, token); err == nil {
				where := "the OS keychain"
				if src == keystore.FromFile {
					where = filepath.Join(h, keystore.KeysDir) + " (no usable keychain)"
				}
				fmt.Printf("Token stored in %s. Clients only need: centrext mcp --agent %s\n", where, ag.ID)
				return nil
			}
		}
		fmt.Printf("\nToken (shown once; put it in CENTREXT_TOKEN in the client's user-level config, never in a repo):\n%s\n", token)
		return nil
	case "grant":
		fs := newFlags("agent grant")
		all := fs.Bool("all", false, "grant all namespaces")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if err := need(pos, 1, "AGENT_ID"); err != nil {
			return err
		}
		nsID, label := "", "all namespaces"
		if !*all {
			if err := need(pos, 2, "NAMESPACE (or --all)"); err != nil {
				return err
			}
			ns, err := a.svc.FindNamespace(ctx, a.user, pos[1])
			if err != nil {
				return err
			}
			nsID, label = ns.ID, ns.Name
		}
		if err := a.svc.GrantAgent(ctx, a.user, pos[0], nsID); err != nil {
			return err
		}
		fmt.Printf("Granted %s access to %s\n", pos[0], label)
		return nil
	case "list":
		agents, err := a.svc.ListAgents(ctx, a.user)
		if err != nil {
			return err
		}
		w := table()
		fmt.Fprintln(w, "ID\tNAME\tCLIENT\tLAST SEEN\tSTATUS")
		for _, ag := range agents {
			seen, status := "never", "active"
			if ag.LastSeenAt != nil {
				seen = ts(*ag.LastSeenAt)
			}
			if ag.RevokedAt != nil {
				status = "revoked " + ts(*ag.RevokedAt)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ag.ID, ag.Name, ag.Client, seen, status)
		}
		return w.Flush()
	case "revoke":
		fs := newFlags("agent revoke")
		reject := fs.Bool("reject-pending", false, "also reject the agent's pending proposals")
		pos, err := parse(fs, args[1:])
		if err != nil {
			return err
		}
		if err := need(pos, 1, "AGENT_ID"); err != nil {
			return err
		}
		n, err := a.svc.RevokeAgent(ctx, a.user, pos[0], *reject)
		if err != nil {
			return err
		}
		if h, err := home(); err == nil {
			_ = keystore.DeleteAgentToken(h, pos[0])
		}
		fmt.Printf("Revoked %s", pos[0])
		if *reject {
			fmt.Printf("; rejected %d pending proposals", n)
		}
		fmt.Println()
		return nil
	}
	return fmt.Errorf("unknown agent command %q", args[0])
}

// ---- stats

// cmdStats prints the capture metrics (ADR-015). The key number is how many
// accepted items another agent reported using.
func (a *app) cmdStats(ctx context.Context) error {
	rows, err := a.svc.CaptureStats(ctx, a.user)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("Nothing saved yet.")
		return nil
	}
	w := table()
	fmt.Fprintln(w, "ORIGIN\tSTATUS\tSAVES\tRETRIEVED BY ANOTHER AGENT\tUSED\tUSED BY ANOTHER AGENT")
	accepted, usedByOther := 0, 0
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\n", r.Origin, r.Status, r.Proposals, r.RetrievedByOther, r.Used, r.UsedByOther)
		if r.Status == domain.ProposalAccepted && r.Origin != domain.OriginUser {
			accepted += r.Proposals
			usedByOther += r.UsedByOther
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if accepted > 0 {
		fmt.Printf("\nAccepted agent saves later used by another agent: %d of %d (%.0f%%)\n", usedByOther, accepted, 100*float64(usedByOther)/float64(accepted))
	}
	fmt.Println("\"Used\" is what agents report with context_used; Centrext never reads conversations.")
	return nil
}

// ---- export

// cmdExport writes the repo namespace's user-verified decisions to
// .centrext-export/ (ADR-020 experiment, ADR-022).
func (a *app) cmdExport(ctx context.Context, args []string) error {
	fs := newFlags("export")
	dir := fs.String("dir", "", "repo to export into (default: this repo)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	start := *dir
	if start == "" {
		start, _ = os.Getwd()
	}
	root := workspace.RepoRoot(start)
	if root == "" {
		return fmt.Errorf("%s isn't in a git repo; export writes files for a repo namespace", start)
	}
	id, err := workspace.Resolve(root)
	if err != nil {
		return err
	}
	ns, err := a.svc.LookupNamespace(ctx, a.user, id)
	if errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("no namespace for this repo yet; nothing has been saved for it")
	}
	if err != nil {
		return err
	}
	ns, views, err := a.svc.ExportSnapshot(ctx, a.user, ns.ID)
	if err != nil {
		return err
	}
	res, err := export.Write(root, ns, views)
	if err != nil {
		return err
	}
	fmt.Printf("Exported %d item(s) from namespace %s to %s\n", len(views), ns.Name, filepath.Join(root, export.Dir))
	for _, w := range res.Written {
		fmt.Println("  wrote   " + w)
	}
	for _, r := range res.Removed {
		fmt.Println("  removed " + r)
	}
	if len(res.Written)+len(res.Removed) == 0 {
		fmt.Println("  already up to date")
	}
	fmt.Println("Only user_verified items are exported. Review the files, then commit them.")
	return nil
}
