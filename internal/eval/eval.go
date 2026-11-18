// Package eval measures retrieval quality on a labeled dataset (section 7,
// ADR-012): precision@k, recall@k, hit@1, MRR, no-answer accuracy and the
// stale-retrieval rate, overall and per query category. Ranking changes in 0.3
// must be judged by these numbers, not by feel.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Dataset is a labeled retrieval set.
type Dataset struct {
	About      string    `json:"about"`
	Namespaces []string  `json:"namespaces"` // created before contexts ("Personal" exists already)
	Contexts   []Context `json:"contexts"`
	Queries    []Query   `json:"queries"`
}

// Context is one item of the corpus. Items are created in file order.
type Context struct {
	Key        string `json:"key"`
	NS         string `json:"ns"`
	Type       string `json:"type"`
	Trust      string `json:"trust"` // user_verified | session_confirmed | agent_reported
	Title      string `json:"title"`
	Body       string `json:"body"`
	AgeDays    int    `json:"age_days,omitempty"`
	Supersedes string `json:"supersedes,omitempty"` // key of the item this replaces
	Deleted    bool   `json:"deleted,omitempty"`
	// Hidden marks items that must never be returned (superseded, deleted or
	// expired). Returning one counts as stale retrieval.
	Hidden bool `json:"hidden,omitempty"`
}

// Query is one labeled query. An empty Relevant list means nothing should be returned.
type Query struct {
	Q        string   `json:"q"`
	NS       string   `json:"ns"`
	Relevant []string `json:"relevant"`
	Cat      string   `json:"cat"`
}

// Load reads and checks a dataset.
func Load(path string) (Dataset, error) {
	var d Dataset
	b, err := os.ReadFile(path)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	keys := map[string]bool{}
	for _, c := range d.Contexts {
		if keys[c.Key] {
			return d, fmt.Errorf("duplicate context key %q", c.Key)
		}
		if c.Supersedes != "" && !keys[c.Supersedes] {
			return d, fmt.Errorf("%s supersedes %q, which must come earlier", c.Key, c.Supersedes)
		}
		keys[c.Key] = true
	}
	for _, q := range d.Queries {
		for _, r := range q.Relevant {
			if !keys[r] {
				return d, fmt.Errorf("query %q: unknown relevant key %q", q.Q, r)
			}
		}
	}
	return d, nil
}

// Result is what the system returned for one query, as context keys in rank order.
type Result struct {
	Query    Query
	Returned []string
}

// Metrics aggregates results. Precision is over the results actually
// returned (at most K): a system that returns one correct item scores 1.0,
// not 0.2. Queries with no relevant item only count toward NoAnswer.
type Metrics struct {
	Queries      int
	Answerable   int
	Precision    float64 // mean precision@K over answerable queries
	Recall       float64 // mean recall@K over answerable queries
	Hit1         float64 // share of answerable queries whose top result is relevant
	MRR          float64 // mean reciprocal rank of the first relevant result
	NoAnswer     int     // queries with nothing relevant
	NoAnswerOK   int     // ... that returned nothing
	StaleQueries int     // queries whose results include a hidden item
}

// K is the cut-off for all metrics.
const K = 5

// Score computes metrics overall ("all") and per category.
func Score(results []Result, hidden map[string]bool) map[string]*Metrics {
	out := map[string]*Metrics{}
	sums := map[string]*Metrics{}
	for _, r := range results {
		for _, key := range []string{"all", r.Query.Cat} {
			m := out[key]
			if m == nil {
				m, sums[key] = &Metrics{}, &Metrics{}
				out[key] = m
			}
			add(m, sums[key], r, hidden)
		}
	}
	for key, m := range out {
		if m.Answerable > 0 {
			s, n := sums[key], float64(m.Answerable)
			m.Precision, m.Recall, m.Hit1, m.MRR = s.Precision/n, s.Recall/n, s.Hit1/n, s.MRR/n
		}
	}
	return out
}

func add(m, sum *Metrics, r Result, hidden map[string]bool) {
	m.Queries++
	top := r.Returned
	if len(top) > K {
		top = top[:K]
	}
	for _, k := range top {
		if hidden[k] {
			m.StaleQueries++
			break
		}
	}
	if len(r.Query.Relevant) == 0 {
		m.NoAnswer++
		if len(top) == 0 {
			m.NoAnswerOK++
		}
		return
	}
	m.Answerable++
	rel := map[string]bool{}
	for _, k := range r.Query.Relevant {
		rel[k] = true
	}
	found := 0
	for i, k := range top {
		if rel[k] {
			if found == 0 {
				sum.MRR += 1 / float64(i+1)
				if i == 0 {
					sum.Hit1++
				}
			}
			found++
		}
	}
	if len(top) > 0 {
		sum.Precision += float64(found) / float64(len(top))
	}
	sum.Recall += float64(found) / float64(len(rel))
}

// Report formats metrics as a table, "all" first, then categories by name.
func Report(ms map[string]*Metrics) string {
	keys := make([]string, 0, len(ms))
	for k := range ms {
		if k != "all" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	keys = append([]string{"all"}, keys...)
	var b strings.Builder
	fmt.Fprintf(&b, "%-11s %7s %7s %7s %7s %7s %9s %7s\n", "category", "queries", "P@5", "R@5", "hit@1", "MRR", "no-answer", "stale")
	for _, k := range keys {
		m := ms[k]
		na := "-"
		if m.NoAnswer > 0 {
			na = fmt.Sprintf("%d/%d", m.NoAnswerOK, m.NoAnswer)
		}
		if m.Answerable == 0 {
			fmt.Fprintf(&b, "%-11s %7d %7s %7s %7s %7s %9s %7d\n", k, m.Queries, "-", "-", "-", "-", na, m.StaleQueries)
			continue
		}
		fmt.Fprintf(&b, "%-11s %7d %6.0f%% %6.0f%% %6.0f%% %7.2f %9s %7d\n", k, m.Queries,
			100*m.Precision, 100*m.Recall, 100*m.Hit1, m.MRR, na, m.StaleQueries)
	}
	return b.String()
}

// LoadQueries reads extra labeled queries (e.g. prompts) for a dataset's corpus.
func LoadQueries(path string, d Dataset) ([]Query, error) {
	var f struct {
		Queries []Query `json:"queries"`
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, c := range d.Contexts {
		keys[c.Key] = true
	}
	for _, q := range f.Queries {
		for _, r := range q.Relevant {
			if !keys[r] {
				return nil, fmt.Errorf("query %q: unknown relevant key %q", q.Q, r)
			}
		}
	}
	return f.Queries, nil
}

// Injection scores per-prompt context injection (ADR-024), where precision
// matters most because nobody asked for the context.
type Injection struct {
	Prompts, WithAnswer, Hits int // Hits: prompts with an answer where a relevant item was injected
	NoAnswer, NoAnswerOK      int // prompts with no answer where nothing was injected
	Injected, Relevant, Stale int // items injected; how many were relevant; how many hidden
}

// ScoreInjection scores prompt results.
func ScoreInjection(results []Result, hidden map[string]bool) Injection {
	var m Injection
	for _, r := range results {
		m.Prompts++
		rel := map[string]bool{}
		for _, k := range r.Query.Relevant {
			rel[k] = true
		}
		hit := false
		for _, k := range r.Returned {
			m.Injected++
			if rel[k] {
				m.Relevant++
				hit = true
			}
			if hidden[k] {
				m.Stale++
			}
		}
		if len(rel) == 0 {
			m.NoAnswer++
			if len(r.Returned) == 0 {
				m.NoAnswerOK++
			}
		} else {
			m.WithAnswer++
			if hit {
				m.Hits++
			}
		}
	}
	return m
}

// Precision is the share of injected items that were relevant (1 if none were injected).
func (m Injection) Precision() float64 {
	if m.Injected == 0 {
		return 1
	}
	return float64(m.Relevant) / float64(m.Injected)
}

func (m Injection) String() string {
	return fmt.Sprintf("precision %.0f%% (%d/%d items), hit rate %d/%d prompts, silent on %d/%d no-answer prompts, stale %d",
		100*m.Precision(), m.Relevant, m.Injected, m.Hits, m.WithAnswer, m.NoAnswerOK, m.NoAnswer, m.Stale)
}
