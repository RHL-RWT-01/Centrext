package application

import (
	"context"
	"slices"

	"centrext/internal/domain"
	"centrext/internal/retrieval"
)

// PromptQuery asks for context worth adding to a user's prompt unasked (ADR-024).
type PromptQuery struct {
	Prompt      string
	NamespaceID string
	Exclude     []string // context ids already delivered this session
	Limit       int      // default 1
	TokenBudget int      // default 400
	MinTerms    int      // default DefaultMinTerms
}

// DefaultMinTerms is the relevance gate: an item must match at least this
// many distinct prompt terms. On the prompt eval (internal/eval), 1 injected
// something on 6 of 14 prompts with no answer; 2 stayed silent on all of them
// at 93% precision; 3 found almost nothing (2 of 20).
const DefaultMinTerms = 2

// promptTrust: unsolicited context must be at least session-confirmed.
var promptTrust = []domain.Trust{domain.TrustUserVerified, domain.TrustTrustedSource, domain.TrustSessionConfirmed}

// PromptContext returns the few items clearly relevant to a prompt, or none.
// It is stricter than Search because nobody asked: lexical only (the prompt
// never leaves the device), agent_reported items excluded, and each item must
// match several prompt terms.
func (s *Service) PromptContext(ctx context.Context, actor domain.Actor, q PromptQuery) (SearchResult, error) {
	if q.Limit <= 0 {
		// On the prompt eval, a second or third item never added a relevant
		// answer and cut precision from 93% to 82%.
		q.Limit = 1
	}
	if q.TokenBudget <= 0 {
		q.TokenBudget = 400
	}
	if q.MinTerms <= 0 {
		q.MinTerms = DefaultMinTerms
	}
	var res SearchResult
	terms := retrieval.PromptTerms(q.Prompt)
	if len(terms) < q.MinTerms {
		return res, nil
	}
	fn := func(tx domain.Tx) error {
		nsIDs, err := accessibleNamespaces(tx, actor, q.NamespaceID)
		if err != nil {
			return err
		}
		now := s.now()
		hits, err := tx.SearchContexts(domain.SearchFilter{Match: retrieval.MatchTerms(terms), NamespaceIDs: nsIDs,
			NotExpiredAt: now, Limit: 20})
		if err != nil {
			return err
		}
		views := map[string]ContextView{}
		var cands []retrieval.Candidate
		for _, h := range hits {
			if !slices.Contains(promptTrust, h.Context.HeadTrust) || slices.Contains(q.Exclude, h.Context.ID) {
				continue
			}
			v, err := s.loadView(tx, h.Context)
			if err != nil {
				return err
			}
			if retrieval.MatchedTerms(terms, v.Head.Title+" "+v.Head.Body) < q.MinTerms {
				continue
			}
			views[h.Context.ID] = v
			cands = append(cands, retrieval.Candidate{Context: h.Context, Lexical: h.Lexical, HasLexical: true})
		}
		for _, r := range s.ranking.Rank(cands) {
			if len(res.Items) == q.Limit {
				res.Truncated = true
				break
			}
			v := views[r.Hit.Context.ID]
			tokens := retrieval.EstimateTokens(v.Head.Title) + retrieval.EstimateTokens(v.Head.Body) + 12
			if res.TokensUsed+tokens > q.TokenBudget {
				res.Truncated = true
				continue
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
