// Package retrieval holds query building, ranking and token budgeting
// (ADR-012). It has no I/O. Authorization happens before anything reaches
// here, and is never part of ranking.
package retrieval

import (
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"centrext/internal/domain"
)

const maxQueryTerms = 12

// BuildMatch turns free text into a safe FTS5 expression: each word becomes a
// quoted prefix term, OR-ed together, so user text can never inject FTS syntax.
func BuildMatch(query string) string {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	seen := map[string]bool{}
	var terms []string
	for _, w := range words {
		if utf8.RuneCountInString(w) < 2 || seen[w] || stopwords[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"*`)
		if len(terms) == maxQueryTerms {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "what": true, "how": true, "are": true,
	"is": true, "of": true, "to": true, "in": true, "on": true, "we": true, "do": true, "our": true,
	"a": true, "an": true, "it": true, "be": true, "or": true, "at": true, "by": true, "this": true,
	"that": true, "should": true, "use": true, "using": true, "which": true,
}

// TrustWeight scales scores so better-trusted items rank higher (ADR-013:
// session_confirmed ranks below user_verified).
func TrustWeight(t domain.Trust) float64 {
	switch t {
	case domain.TrustUserVerified:
		return 1.0
	case domain.TrustTrustedSource:
		return 0.9
	case domain.TrustSessionConfirmed:
		return 0.8
	case domain.TrustAgentReported:
		return 0.5
	}
	return 0.2
}

// EstimateTokens is a rough token count (about 4 characters per token).
func EstimateTokens(s string) int {
	return int(math.Ceil(float64(utf8.RuneCountInString(s)) / 4))
}

// Scored is one ranked candidate.
type Scored struct {
	Hit   domain.SearchHit
	Score float64
}

// Rank orders hits by lexical score (normalized to 0..1) times trust weight.
// This is the 0.2 baseline; semantic, freshness and importance join in 0.3.
func Rank(hits []domain.SearchHit) []Scored {
	maxLex := 0.0
	for _, h := range hits {
		maxLex = math.Max(maxLex, h.Lexical)
	}
	out := make([]Scored, len(hits))
	for i, h := range hits {
		lex := 0.0
		if maxLex > 0 {
			lex = h.Lexical / maxLex
		}
		out[i] = Scored{Hit: h, Score: math.Round(lex*TrustWeight(h.Context.HeadTrust)*1000) / 1000}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 0 && MinRelativeScore > 0 {
		cut := out[0].Score * MinRelativeScore
		for i, s := range out {
			if s.Score < cut {
				return out[:i]
			}
		}
	}
	return out
}

// MinRelativeScore drops results scoring below this fraction of the top
// result: OR-matched prefix terms otherwise return items that share one
// common word. Chosen with the eval harness (internal/eval): 0.4 raised P@5
// from 71% to 83% with R@5 unchanged at 90%; above it, multi-answer recall
// drops. Tuned on a small synthetic set, so re-check it on real queries.
const MinRelativeScore = 0.4
