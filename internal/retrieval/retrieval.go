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
	cands := make([]Candidate, len(hits))
	for i, h := range hits {
		cands[i] = Candidate{Context: h.Context, Lexical: h.Lexical, HasLexical: true}
	}
	return HybridRank(cands)
}

// Candidate is one item found lexically, semantically, or both.
type Candidate struct {
	Context    domain.Context
	Lexical    float64 // bm25-derived, higher is better
	HasLexical bool
	Cosine     float64 // similarity to the query vector
	HasVector  bool    // the item has a current vector (its namespace allows embeddings)
}

// Weights are the hybrid ranking parameters (ADR-012, ADR-023).
type Weights struct {
	Semantic, Lexical float64 // relative weights; trust is applied as a multiplier
	// CosineFloor..CosineCeil maps a model's typical similarities to 0..1:
	// at or below the floor there is no semantic credit, at the ceiling full credit.
	CosineFloor, CosineCeil float64
	MinCosine               float64 // semantic-only candidates need at least this
	// MinRelative drops results scoring below this fraction of the top result:
	// OR-matched prefix terms otherwise return items that share one common
	// word. Chosen with the eval harness (internal/eval): 0.4 raised lexical
	// P@5 from 71% to 83% with R@5 unchanged at 90%; above it, multi-answer
	// recall drops. Tuned on a small synthetic set, so re-check it on real queries.
	MinRelative float64
}

// DefaultWeights start from ADR-012's hypothesis (0.40 semantic, 0.25
// lexical). Cosine ranges differ by model; tune them on the eval harness with
// recorded vectors (internal/eval).
var DefaultWeights = Weights{Semantic: 0.40, Lexical: 0.25, CosineFloor: 0.20, CosineCeil: 0.70, MinCosine: 0.35, MinRelative: 0.4}

// HybridRank ranks with DefaultWeights.
func HybridRank(cands []Candidate) []Scored { return DefaultWeights.Rank(cands) }

// Rank scores candidates and sorts them, best first. Candidates without a
// lexical hit need a vector with cosine >= MinCosine. Items with a vector
// blend both signals; items without one (local_only namespaces, or before
// they're indexed) score on lexical alone.
func (w Weights) Rank(cands []Candidate) []Scored {
	maxLex := 0.0
	for _, c := range cands {
		if c.HasLexical {
			maxLex = math.Max(maxLex, c.Lexical)
		}
	}
	out := make([]Scored, 0, len(cands))
	for _, c := range cands {
		if !c.HasLexical && (!c.HasVector || c.Cosine < w.MinCosine) {
			continue
		}
		lex := 0.0
		if c.HasLexical && maxLex > 0 {
			lex = c.Lexical / maxLex
		}
		score := lex
		if c.HasVector {
			sem := math.Min(1, math.Max(0, (c.Cosine-w.CosineFloor)/(w.CosineCeil-w.CosineFloor)))
			score = (w.Semantic*sem + w.Lexical*lex) / (w.Semantic + w.Lexical)
		}
		out = append(out, Scored{Hit: domain.SearchHit{Context: c.Context, Lexical: c.Lexical},
			Score: math.Round(score*TrustWeight(c.Context.HeadTrust)*1000) / 1000})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > 0 && w.MinRelative > 0 {
		cut := out[0].Score * w.MinRelative
		for i, s := range out {
			if s.Score < cut {
				return out[:i]
			}
		}
	}
	return out
}
