package application

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"centrext/internal/domain"
	"centrext/internal/policy"
	"centrext/internal/retrieval"
)

// SearchQuery is the input to Search.
type SearchQuery struct {
	Query       string
	NamespaceID string // empty: every namespace the actor can access
	Types       []domain.ContextType
	Limit       int // default 5
	TokenBudget int // default 1500
}

// SearchItem is one result with its score and size.
type SearchItem struct {
	ContextView
	Score  float64
	Tokens int
}

// SearchResult is what Search returns: structured records, not a prompt.
type SearchResult struct {
	Items      []SearchItem
	TokensUsed int
	Truncated  bool // more matches existed than fit the limit or budget
}

// accessibleNamespaces returns the namespaces to search: one if requested
// (after an access check), or all the actor can access.
func accessibleNamespaces(tx domain.Tx, actor domain.Actor, requested string) ([]string, error) {
	grants, err := grantsFor(tx, actor)
	if err != nil {
		return nil, err
	}
	if requested != "" {
		if err := policy.CanAccessNamespace(actor, requested, grants); err != nil {
			return nil, err
		}
		return []string{requested}, nil
	}
	all, err := tx.ListNamespaces()
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, n := range all {
		if policy.CanAccessNamespace(actor, n.ID, grants) == nil {
			ids = append(ids, n.ID)
		}
	}
	return ids, nil
}

// Search runs full-text search over active context the actor may read.
// Authorization filters candidates before ranking (ADR-012).
func (s *Service) Search(ctx context.Context, actor domain.Actor, q SearchQuery) (SearchResult, error) {
	if q.Limit <= 0 {
		q.Limit = 5
	}
	if q.Limit > 50 {
		q.Limit = 50
	}
	if q.TokenBudget <= 0 {
		q.TokenBudget = 1500
	}
	match := retrieval.BuildMatch(q.Query)
	if match == "" {
		return SearchResult{}, domain.Invalidf("query has no searchable words")
	}
	for _, t := range q.Types {
		if !t.Valid() {
			return SearchResult{}, domain.Invalidf("unknown context type %q", t)
		}
	}
	// Network work happens before the transaction (ADR-023).
	qvec := s.queryVector(ctx, actor, q)
	var res SearchResult
	fn := func(tx domain.Tx) error {
		nsIDs, err := accessibleNamespaces(tx, actor, q.NamespaceID)
		if err != nil {
			return err
		}
		now := s.now()
		hits, err := tx.SearchContexts(domain.SearchFilter{Match: match, NamespaceIDs: nsIDs, Types: q.Types,
			NotExpiredAt: now, Limit: q.Limit * 4})
		if err != nil {
			return err
		}
		cands, err := s.candidates(tx, nsIDs, hits, qvec, q, now)
		if err != nil {
			return err
		}
		ranked := s.ranking.Rank(cands)
		for _, r := range ranked {
			if len(res.Items) == q.Limit {
				res.Truncated = true
				break
			}
			v, err := s.loadView(tx, r.Hit.Context)
			if err != nil {
				return err
			}
			tokens := retrieval.EstimateTokens(v.Head.Title) + retrieval.EstimateTokens(v.Head.Body) + 12
			if res.TokensUsed+tokens > q.TokenBudget {
				res.Truncated = true
				if len(res.Items) > 0 {
					break
				}
				continue // a single item bigger than the budget is skipped
			}
			res.Items = append(res.Items, SearchItem{ContextView: v, Score: r.Score, Tokens: tokens})
			res.TokensUsed += tokens
		}
		if actor.IsUser() {
			return nil
		}
		for _, it := range res.Items {
			if err := tx.RecordUsage(domain.UsageEvent{ID: newID("use", now), At: now, AgentID: actor.ID,
				ContextID: it.Context.ID, Version: it.Context.CurrentVersion, Kind: domain.UsageRetrieved}); err != nil {
				return err
			}
		}
		return nil
	}
	if actor.IsUser() {
		return res, s.store.Read(ctx, fn)
	}
	return res, s.store.InTx(ctx, fn)
}

// Types that go in the session summary. Q1 names decisions and conventions;
// architecture items are included because the core scenario's "database
// architecture decision" may be stored as either type.
var summaryTypes = []domain.ContextType{domain.TypeDecision, domain.TypeArchitecture, domain.TypeConvention}

// summaryTrust: agent_reported items never go in the summary (ADR-014).
var summaryTrust = []domain.Trust{domain.TrustUserVerified, domain.TrustTrustedSource, domain.TrustSessionConfirmed}

const summaryBodyChars = 400

// Summary renders the session-start summary for a namespace within a token
// budget (ADR-014). It is delivered by hooks, rules files and MCP server
// instructions.
func (s *Service) Summary(ctx context.Context, actor domain.Actor, namespaceID string, budget int) (string, error) {
	out, _, err := s.SummaryWithIDs(ctx, actor, namespaceID, budget)
	return out, err
}

// SummaryWithIDs is Summary plus the ids of the items it shows, so later
// deliveries in the same session can skip them (ADR-024).
func (s *Service) SummaryWithIDs(ctx context.Context, actor domain.Actor, namespaceID string, budget int) (string, []string, error) {
	var ids []string
	if budget <= 0 {
		budget = 1000
	}
	var out strings.Builder
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		ns, err := tx.GetNamespace(namespaceID)
		if err != nil {
			return err
		}
		grants, err := grantsFor(tx, actor)
		if err != nil {
			return err
		}
		if err := policy.CanAccessNamespace(actor, ns.ID, grants); err != nil {
			return err
		}
		now := s.now()
		cs, err := tx.ListContexts(domain.ContextFilter{NamespaceIDs: []string{ns.ID}, Types: summaryTypes,
			Statuses: []domain.ContextStatus{domain.StatusActive}, NotExpiredAt: &now})
		if err != nil {
			return err
		}
		cs = slices.DeleteFunc(cs, func(c domain.Context) bool { return !slices.Contains(summaryTrust, c.HeadTrust) })
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].HeadTrust.Rank() != cs[j].HeadTrust.Rank() {
				return cs[i].HeadTrust.Rank() > cs[j].HeadTrust.Rank()
			}
			return cs[i].UpdatedAt.After(cs[j].UpdatedAt)
		})

		fmt.Fprintf(&out, "Centrext shared context for %q. This was saved earlier, possibly through another AI tool; prefer it over assumptions.\n", ns.Name)
		used := retrieval.EstimateTokens(out.String())
		shown := 0
		if len(cs) == 0 {
			out.WriteString("No decisions or conventions are saved for this namespace yet.\n")
		} else {
			out.WriteString("\nDecisions and conventions:\n")
		}
		for _, c := range cs {
			head, err := tx.GetRevision(c.ID, c.CurrentVersion)
			if err != nil {
				return err
			}
			body := strings.Join(strings.Fields(head.Body), " ")
			if utf8.RuneCountInString(body) > summaryBodyChars {
				body = string([]rune(body)[:summaryBodyChars]) + "…"
			}
			line := fmt.Sprintf("- [%s, %s] %s: %s (id %s, v%d)\n", c.Type, c.HeadTrust, head.Title, body, c.ID, c.CurrentVersion)
			cost := retrieval.EstimateTokens(line)
			if used+cost > budget-60 { // keep room for the footer
				break
			}
			out.WriteString(line)
			used += cost
			shown++
			ids = append(ids, c.ID)
		}
		if hidden := len(cs) - shown; hidden > 0 {
			fmt.Fprintf(&out, "(%d more not shown.)\n", hidden)
		}
		unreviewed, err := tx.ListContexts(domain.ContextFilter{NamespaceIDs: []string{ns.ID}, Unreviewed: true,
			Statuses: []domain.ContextStatus{domain.StatusActive}, NotExpiredAt: &now})
		if err != nil {
			return err
		}
		pending, err := tx.CountPendingProposals(ns.ID)
		if err != nil {
			return err
		}
		out.WriteString("\nUse context_search before choosing schemas, APIs, dependencies or conventions; there may be more saved context.")
		if n := len(unreviewed) + pending; n > 0 {
			fmt.Fprintf(&out, " %d item(s) await the user's review (`centrext review`).", n)
		}
		out.WriteString("\n")
		return nil
	})
	return out.String(), ids, err
}
