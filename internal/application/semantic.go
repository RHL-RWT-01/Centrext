package application

import (
	"context"
	"sort"
	"time"

	"centrext/internal/domain"
	"centrext/internal/infrastructure/embeddings"
	"centrext/internal/policy"
	"centrext/internal/retrieval"
)

// SemanticTimeout bounds the API work one search may do (indexing plus the
// query vector). Past it, the search is lexical only (ADR-023).
const SemanticTimeout = 4 * time.Second

// IndexBatch is how many missing vectors one search computes at most.
const IndexBatch = 64

// WithEmbedder enables semantic search through a hosted embeddings API.
func WithEmbedder(e domain.Embedder) Option { return func(s *Service) { s.embedder = e } }

// WithRanking overrides the hybrid ranking weights (eval tuning).
func WithRanking(w retrieval.Weights) Option { return func(s *Service) { s.ranking = w } }

// WithWarn receives problems that don't fail an operation, such as an
// embeddings API error that made a search fall back to lexical.
func WithWarn(f func(msg string)) Option { return func(s *Service) { s.warn = f } }

func (s *Service) warnf(msg string) {
	if s.warn != nil {
		s.warn(msg)
	}
}

// Embedder returns the configured embedder, or nil.
func (s *Service) Embedder() domain.Embedder { return s.embedder }

// eligibleForEmbedding keeps the namespaces whose policy lets content leave
// the device. local_only content is never sent to an API (ADR-009, ADR-023).
func eligibleForEmbedding(tx domain.Tx, nsIDs []string) ([]string, error) {
	var out []string
	for _, id := range nsIDs {
		ns, err := tx.GetNamespace(id)
		if err != nil {
			return nil, err
		}
		if ns.StorageMode != domain.StorageLocalOnly {
			out = append(out, id)
		}
	}
	return out, nil
}

func docText(j domain.EmbedJob) string { return j.Title + "\n\n" + j.Body }

// indexMissing computes vectors for up to limit items in the namespaces. The
// API call happens outside any transaction.
func (s *Service) indexMissing(ctx context.Context, nsIDs []string, limit int) (int, error) {
	var jobs []domain.EmbedJob
	model := s.embedder.Model()
	if err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		jobs, err = tx.MissingEmbeddings(model, nsIDs, s.now(), limit)
		return err
	}); err != nil || len(jobs) == 0 {
		return 0, err
	}
	texts := make([]string, len(jobs))
	for i, j := range jobs {
		texts[i] = docText(j)
	}
	vecs, err := s.embedder.Embed(ctx, texts, domain.EmbedDocument)
	if err != nil {
		return 0, err
	}
	err = s.store.InTx(ctx, func(tx domain.Tx) error {
		now := s.now()
		for i, j := range jobs {
			if err := tx.PutEmbedding(j.ContextID, j.Version, model, vecs[i], now); err != nil {
				return err
			}
		}
		return nil
	})
	return len(jobs), err
}

// queryVector prepares semantic search: it indexes missing items in the
// eligible namespaces the actor may search, then embeds the query. It returns
// nil when semantic search doesn't apply or anything fails.
func (s *Service) queryVector(ctx context.Context, actor domain.Actor, q SearchQuery) []float32 {
	if s.embedder == nil || policy.DetectSecret(q.Query) != "" {
		return nil
	}
	var eligible []string
	if err := s.store.Read(ctx, func(tx domain.Tx) error {
		nsIDs, err := accessibleNamespaces(tx, actor, q.NamespaceID)
		if err != nil {
			return err
		}
		eligible, err = eligibleForEmbedding(tx, nsIDs)
		return err
	}); err != nil || len(eligible) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, SemanticTimeout)
	defer cancel()
	if _, err := s.indexMissing(ctx, eligible, IndexBatch); err != nil {
		s.warnf("embeddings: indexing failed, using lexical search: " + err.Error())
		return nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{q.Query}, domain.EmbedQuery)
	if err != nil || len(vecs) != 1 {
		if err != nil {
			s.warnf("embeddings: query failed, using lexical search: " + err.Error())
		}
		return nil
	}
	return vecs[0]
}

// candidates merges lexical hits with semantic matches from eligible
// namespaces. Called inside the search transaction, after authorization.
func (s *Service) candidates(tx domain.Tx, nsIDs []string, hits []domain.SearchHit, qvec []float32, q SearchQuery, now time.Time) ([]retrieval.Candidate, error) {
	byID := map[string]*retrieval.Candidate{}
	var order []string
	for _, h := range hits {
		byID[h.Context.ID] = &retrieval.Candidate{Context: h.Context, Lexical: h.Lexical, HasLexical: true}
		order = append(order, h.Context.ID)
	}
	if qvec != nil {
		eligible, err := eligibleForEmbedding(tx, nsIDs)
		if err != nil {
			return nil, err
		}
		embedded, err := tx.EmbeddedContexts(domain.EmbeddingFilter{Model: s.embedder.Model(), NamespaceIDs: eligible, Types: q.Types, NotExpiredAt: now})
		if err != nil {
			return nil, err
		}
		type sim struct {
			c   domain.Context
			cos float64
		}
		sims := make([]sim, 0, len(embedded))
		for _, e := range embedded {
			sims = append(sims, sim{e.Context, embeddings.Cosine(qvec, e.Vector)})
		}
		sort.Slice(sims, func(i, j int) bool { return sims[i].cos > sims[j].cos })
		for i, sm := range sims {
			if c, ok := byID[sm.c.ID]; ok {
				c.Cosine, c.HasVector = sm.cos, true
				continue
			}
			if i < q.Limit*4 && sm.cos >= s.ranking.MinCosine {
				byID[sm.c.ID] = &retrieval.Candidate{Context: sm.c, Cosine: sm.cos, HasVector: true}
				order = append(order, sm.c.ID)
			}
		}
	}
	out := make([]retrieval.Candidate, len(order))
	for i, id := range order {
		out[i] = *byID[id]
	}
	return out, nil
}

// EmbeddingStatus reports semantic coverage for the user.
type EmbeddingStatus struct {
	Model     string
	Eligible  []domain.Namespace // policy allows embeddings
	LocalOnly []domain.Namespace // never embedded
	Active    int                // active items in eligible namespaces
	Embedded  int                // ... with a current vector
}

// EmbeddingStatus returns coverage. With sync set, it first indexes every
// eligible item that lacks a current vector.
func (s *Service) EmbeddingStatus(ctx context.Context, actor domain.Actor, sync bool) (EmbeddingStatus, error) {
	var st EmbeddingStatus
	if err := policy.RequireUser(actor, "checking embeddings"); err != nil {
		return st, err
	}
	if s.embedder == nil {
		return st, domain.Invalidf("embeddings aren't enabled; run `centrext embeddings enable`")
	}
	st.Model = s.embedder.Model()
	var ids []string
	if err := s.store.Read(ctx, func(tx domain.Tx) error {
		all, err := tx.ListNamespaces()
		if err != nil {
			return err
		}
		for _, n := range all {
			if n.StorageMode == domain.StorageLocalOnly {
				st.LocalOnly = append(st.LocalOnly, n)
			} else {
				st.Eligible = append(st.Eligible, n)
				ids = append(ids, n.ID)
			}
		}
		return nil
	}); err != nil {
		return st, err
	}
	for sync && len(ids) > 0 {
		n, err := s.indexMissing(ctx, ids, 256)
		if err != nil {
			return st, err
		}
		if n == 0 {
			break
		}
	}
	err := s.store.Read(ctx, func(tx domain.Tx) error {
		var err error
		st.Active, st.Embedded, err = tx.CountEmbeddings(st.Model, ids, s.now())
		return err
	})
	return st, err
}
