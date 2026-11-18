package eval_test

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/eval"
	"centrext/internal/infrastructure/embeddings"
	"centrext/internal/infrastructure/sqlite"
	"centrext/internal/retrieval"
)

// Floors are the baseline measured when the harness was added (lexical FTS5
// x trust, relative score cutoff). The test fails on a regression below them, not for missing the
// section 7 targets (P@5 >= 80%, R@5 >= 75%), which 0.3 has to earn.
// Raise them when ranking improves.
const (
	floorPrecision = 0.82 // measured 0.83 with MinRelative 0.4
	floorRecall    = 0.89 // measured 0.90
)

var base = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// corpus is the dataset loaded into a store through the application layer,
// so supersede, delete and expiry use production paths.
type corpus struct {
	ds     eval.Dataset
	store  *sqlite.Store
	reader domain.Actor
	nsIDs  map[string]string
	keys   map[string]string // context id -> key
	hidden map[string]bool
}

func buildCorpus(t *testing.T) *corpus {
	t.Helper()
	ds, err := eval.Load(filepath.Join("testdata", "retrieval.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "eval.db"), bytes.Repeat([]byte{9}, sqlite.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	now := base
	svc := application.New(store, application.WithClock(func() time.Time { return now }))
	if _, err := svc.Init(ctx, "eval"); err != nil {
		t.Fatal(err)
	}
	user, _ := svc.LocalUser(ctx)
	c := &corpus{ds: ds, store: store, nsIDs: map[string]string{}, keys: map[string]string{}, hidden: map[string]bool{}}
	// Every namespace allows embeddings, so the hybrid runs cover the whole set.
	personal, _ := svc.FindNamespace(ctx, user, application.PersonalNamespace)
	if _, err := svc.SetStorageMode(ctx, user, personal.ID, domain.StorageLocalPrimary); err != nil {
		t.Fatal(err)
	}
	c.nsIDs[application.PersonalNamespace] = personal.ID
	for _, name := range ds.Namespaces {
		ns, err := svc.CreateNamespace(ctx, user, application.NewNamespace{Name: name, StorageMode: domain.StorageLocalPrimary})
		if err != nil {
			t.Fatal(err)
		}
		c.nsIDs[name] = ns.ID
	}
	agent := func(name string) domain.Actor {
		_, tok, err := svc.RegisterAgent(ctx, user, application.RegisterAgentInput{Name: name, Client: name, AllNamespaces: true})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := svc.Authenticate(ctx, tok)
		return a
	}
	writer := agent("writer")
	c.reader = agent("reader")

	ids := map[string]string{} // key -> context id
	for _, cx := range ds.Contexts {
		now = base.Add(-time.Duration(cx.AgeDays) * 24 * time.Hour)
		in := application.ProposeInput{NamespaceID: c.nsIDs[cx.NS], Kind: domain.KindCreate,
			Type: domain.ContextType(cx.Type), Title: cx.Title, Body: cx.Body}
		actor := user
		switch cx.Trust {
		case "user_verified":
			in.Origin = domain.OriginUser
		case "session_confirmed":
			in.Origin, actor = domain.OriginExplicit, writer
		case "agent_reported":
			in.Origin, actor = domain.OriginSilent, writer
		default:
			t.Fatalf("%s: unsupported trust %q", cx.Key, cx.Trust)
		}
		if cx.Supersedes != "" {
			in.Kind, in.TargetContextID, in.BaseVersion, in.Origin, actor = domain.KindSupersede, ids[cx.Supersedes], 1, domain.OriginUser, user
		}
		res, err := svc.Propose(ctx, actor, in)
		if err != nil || res.Context == nil {
			t.Fatalf("%s: %v (%+v)", cx.Key, err, res.Proposal)
		}
		id := res.Context.Context.ID
		ids[cx.Key], c.keys[id] = id, cx.Key
		if cx.Deleted {
			if err := svc.DeleteContext(ctx, user, id, res.Context.Context.CurrentVersion); err != nil {
				t.Fatal(err)
			}
		}
		if cx.Hidden {
			c.hidden[cx.Key] = true
		}
	}
	return c
}

// run searches every query as an agent with a service built from opts.
func (c *corpus) run(t *testing.T, opts ...application.Option) (map[string]*eval.Metrics, []string) {
	t.Helper()
	opts = append([]application.Option{application.WithClock(func() time.Time { return base })}, opts...)
	svc := application.New(c.store, opts...)
	var results []eval.Result
	var misses []string
	for _, q := range c.ds.Queries {
		res, err := svc.Search(context.Background(), c.reader, application.SearchQuery{Query: q.Q, NamespaceID: c.nsIDs[q.NS], Limit: eval.K, TokenBudget: 1 << 20})
		if err != nil {
			t.Fatalf("%q: %v", q.Q, err)
		}
		r := eval.Result{Query: q}
		for _, it := range res.Items {
			r.Returned = append(r.Returned, c.keys[it.Context.ID])
		}
		results = append(results, r)
		if len(q.Relevant) > 0 && (len(r.Returned) == 0 || !contains(q.Relevant, r.Returned[0])) {
			misses = append(misses, q.Cat+": "+q.Q+" -> ["+strings.Join(r.Returned, ", ")+"]")
		}
	}
	return eval.Score(results, c.hidden), misses
}

func checkFloors(t *testing.T, all *eval.Metrics) {
	t.Helper()
	if all.StaleQueries > 0 {
		t.Errorf("stale retrieval in %d queries; superseded, deleted and expired items must never be returned", all.StaleQueries)
	}
	if all.Precision < floorPrecision || all.Recall < floorRecall {
		t.Errorf("regression: P@5 %.2f (floor %.2f), R@5 %.2f (floor %.2f)", all.Precision, floorPrecision, all.Recall, floorRecall)
	}
}

// TestRetrievalEval measures lexical search (no embedder). Run with -v to see the table.
func TestRetrievalEval(t *testing.T) {
	c := buildCorpus(t)
	ms, misses := c.run(t)
	t.Logf("lexical: %d contexts, %d queries\n%s", len(c.ds.Contexts), len(c.ds.Queries), eval.Report(ms))
	t.Logf("top result not relevant (%d):\n  %s", len(misses), strings.Join(misses, "\n  "))
	checkFloors(t, ms["all"])
}

// TestHybridPlumbing runs the hybrid path over the whole corpus with a local
// hashed bag-of-words embedder. It checks wiring, not quality (the stand-in
// is a poor model): no stale results through the semantic path, every item
// gets indexed, and keyword queries still find their answer first.
func TestHybridPlumbing(t *testing.T) {
	c := buildCorpus(t)
	ms, _ := c.run(t, application.WithEmbedder(hashEmbedder{}))
	t.Logf("hashed embedder:\n%s", eval.Report(ms))
	if ms["all"].StaleQueries > 0 {
		t.Errorf("stale retrieval in %d queries through the semantic path", ms["all"].StaleQueries)
	}
	if ms["keyword"].Hit1 < 1 {
		t.Errorf("keyword hit@1 %.2f; semantic candidates must not push out exact matches", ms["keyword"].Hit1)
	}
	st, err := application.New(c.store, application.WithEmbedder(hashEmbedder{})).EmbeddingStatus(context.Background(), userOf(t, c), false)
	if err != nil || st.Embedded != st.Active || st.Active == 0 {
		t.Errorf("searches should have indexed every active item: %+v %v", st, err)
	}
}

// TestRetrievalEvalHybrid replays recorded model vectors from
// testdata/vectors/*.json. To record (needs an API key; sends only the
// synthetic dataset):
//
//	CENTREXT_EVAL_RECORD=openai OPENAI_API_KEY=... go test ./internal/eval -run Hybrid -v
//	CENTREXT_EVAL_RECORD=voyage VOYAGE_API_KEY=... go test ./internal/eval -run Hybrid -v
//
// CENTREXT_EVAL_MODEL picks a model other than the provider's default.
// CENTREXT_EVAL_SWEEP=1 also searches the ranking weights and logs the best.
func TestRetrievalEvalHybrid(t *testing.T) {
	if p := os.Getenv("CENTREXT_EVAL_RECORD"); p != "" {
		record(t, p)
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "vectors", "*.json"))
	if len(files) == 0 {
		t.Skip("no recorded vectors in testdata/vectors; see this test's comment to record them")
	}
	c := buildCorpus(t)
	lex, _ := c.run(t)
	for _, f := range files {
		cache, err := eval.LoadVectorCache(f, nil)
		if err != nil {
			t.Fatal(err)
		}
		ms, misses := c.run(t, application.WithEmbedder(cache))
		if cache.Misses() > 0 {
			t.Fatalf("%s is missing vectors for the current dataset; record again", f)
		}
		t.Logf("%s (default weights):\n%s\nlexical for comparison:\n%s", cache.Model(), eval.Report(ms), eval.Report(lex))
		t.Logf("top result not relevant (%d):\n  %s", len(misses), strings.Join(misses, "\n  "))
		checkFloors(t, ms["all"])
		if os.Getenv("CENTREXT_EVAL_SWEEP") != "" {
			sweep(t, c, cache)
		}
	}
}

func record(t *testing.T, provider string) {
	cfg, err := embeddings.Config{Provider: provider, Model: os.Getenv("CENTREXT_EVAL_MODEL")}.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	var key string
	for _, e := range embeddings.KeyEnv(provider) {
		if key = os.Getenv(e); key != "" {
			break
		}
	}
	if key == "" {
		t.Fatalf("set %s to record", strings.Join(embeddings.KeyEnv(provider), " or "))
	}
	live := embeddings.New(cfg, key, 60*time.Second)
	path := filepath.Join("testdata", "vectors", strings.ReplaceAll(live.Model(), "/", "-")+".json")
	cache, err := eval.LoadVectorCache(path, live)
	if err != nil {
		t.Fatal(err)
	}
	c := buildCorpus(t)
	if _, err := application.New(c.store, application.WithEmbedder(cache)).EmbeddingStatus(context.Background(), userOf(t, c), true); err != nil {
		t.Fatal(err)
	}
	c.run(t, application.WithEmbedder(cache)) // records query vectors
	if err := cache.Save(path); err != nil {
		t.Fatal(err)
	}
	t.Logf("recorded %d new vectors to %s", cache.Misses(), path)
}

func userOf(t *testing.T, c *corpus) domain.Actor {
	u, err := application.New(c.store).LocalUser(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// sweep grid-searches the ranking weights on recorded vectors. Choosing
// weights on the same 60 queries they're scored on overfits; treat the result
// as a direction and re-check on real queries.
func sweep(t *testing.T, c *corpus, cache *eval.VectorCache) {
	type row struct {
		w   retrieval.Weights
		all eval.Metrics
		obj float64
	}
	var rows []row
	for _, sem := range []float64{0.25, 0.40, 0.55, 0.70} {
		for _, floor := range []float64{0.10, 0.20, 0.30} {
			for _, ceil := range []float64{0.50, 0.60, 0.70} {
				for _, minCos := range []float64{0.30, 0.40, 0.50} {
					for _, minRel := range []float64{0.3, 0.4, 0.5} {
						w := retrieval.Weights{Semantic: sem, Lexical: 0.25, CosineFloor: floor, CosineCeil: ceil, MinCosine: minCos, MinRelative: minRel}
						ms, _ := c.run(t, application.WithEmbedder(cache), application.WithRanking(w))
						a := *ms["all"]
						if a.StaleQueries > 0 {
							continue
						}
						noAns := 0.0
						if a.NoAnswer > 0 {
							noAns = float64(a.NoAnswerOK) / float64(a.NoAnswer)
						}
						rows = append(rows, row{w, a, (a.Precision + a.Recall + a.Hit1 + 0.5*noAns) / 3.5})
					}
				}
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].obj > rows[j].obj })
	var b strings.Builder
	for _, r := range rows[:min(8, len(rows))] {
		fmt.Fprintf(&b, "  sem %.2f floor %.2f ceil %.2f minCos %.2f minRel %.1f -> P@5 %.2f R@5 %.2f hit@1 %.2f no-answer %d/%d\n",
			r.w.Semantic, r.w.CosineFloor, r.w.CosineCeil, r.w.MinCosine, r.w.MinRelative,
			r.all.Precision, r.all.Recall, r.all.Hit1, r.all.NoAnswerOK, r.all.NoAnswer)
	}
	t.Logf("%s sweep, best of %d:\n%s", cache.Model(), len(rows), b.String())
}

// hashEmbedder hashes words into a fixed-size vector: a local stand-in with
// lexical-like behavior, used only to test the hybrid wiring.
type hashEmbedder struct{}

func (hashEmbedder) Model() string { return "test/hash" }

func (hashEmbedder) Embed(_ context.Context, texts []string, _ domain.EmbedKind) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 256)
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			h := fnv.New32a()
			h.Write([]byte(w))
			v[h.Sum32()%256]++
		}
		var n float64
		for _, x := range v {
			n += float64(x * x)
		}
		for j := range v {
			if n > 0 {
				v[j] /= float32(math.Sqrt(n))
			}
		}
		out[i] = v
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestScore(t *testing.T) {
	q := func(cat string, rel ...string) eval.Query { return eval.Query{Cat: cat, Relevant: rel} }
	ms := eval.Score([]eval.Result{
		{Query: q("a", "x"), Returned: []string{"x", "y"}},      // P 0.5, R 1, hit, RR 1
		{Query: q("a", "x", "z"), Returned: []string{"y", "z"}}, // P 0.5, R 0.5, RR 0.5
		{Query: q("b"), Returned: nil},                          // no-answer ok
		{Query: q("b"), Returned: []string{"old"}},              // no-answer wrong, stale
	}, map[string]bool{"old": true})
	all := ms["all"]
	if all.Queries != 4 || all.Answerable != 2 || all.Precision != 0.5 || all.Recall != 0.75 ||
		all.Hit1 != 0.5 || all.MRR != 0.75 || all.NoAnswer != 2 || all.NoAnswerOK != 1 || all.StaleQueries != 1 {
		t.Fatalf("%+v", *all)
	}
	if ms["b"].Answerable != 0 || ms["a"].Queries != 2 {
		t.Fatal("per-category split is wrong")
	}
}

func TestVectorCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.json")
	rec, err := eval.LoadVectorCache(path, hashEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := rec.Embed(context.Background(), []string{"orders in postgres"}, domain.EmbedDocument)
	if err := rec.Save(path); err != nil {
		t.Fatal(err)
	}
	replay, err := eval.LoadVectorCache(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := replay.Embed(context.Background(), []string{"orders in postgres"}, domain.EmbedDocument)
	if err != nil || replay.Model() != "test/hash" || embeddings.Cosine(got[0], want[0]) < 0.9999 {
		t.Fatalf("replay: %v %v", err, replay.Model())
	}
	if _, err := replay.Embed(context.Background(), []string{"orders in postgres"}, domain.EmbedQuery); err == nil {
		t.Fatal("query and document vectors are cached separately; a miss must fail in replay mode")
	}
}

// Floors for per-prompt injection at the default gate (ADR-024). Precision
// and silence matter more than hit rate: wrong context nobody asked for
// misleads the agent, while a miss leaves context_search as the fallback.
const (
	floorInjectPrecision = 0.85
	floorInjectSilence   = 0.85 // share of no-answer prompts that get nothing
)

// TestPromptInjectionEval measures PromptContext on labeled prompts at
// several gates. Run with -v to see the comparison.
func TestPromptInjectionEval(t *testing.T) {
	c := buildCorpus(t)
	prompts, err := eval.LoadQueries(filepath.Join("testdata", "prompts.json"), c.ds)
	if err != nil {
		t.Fatal(err)
	}
	svc := application.New(c.store, application.WithClock(func() time.Time { return base }))
	var def eval.Injection
	var b strings.Builder
	for _, minTerms := range []int{1, 2, 3} {
		var results []eval.Result
		var wrong []string
		for _, q := range prompts {
			res, err := svc.PromptContext(context.Background(), c.reader, application.PromptQuery{Prompt: q.Q, NamespaceID: c.nsIDs[q.NS], MinTerms: minTerms})
			if err != nil {
				t.Fatal(err)
			}
			r := eval.Result{Query: q}
			for _, it := range res.Items {
				k := c.keys[it.Context.ID]
				r.Returned = append(r.Returned, k)
				if !contains(q.Relevant, k) {
					wrong = append(wrong, q.Q+" -> "+k)
				}
			}
			results = append(results, r)
		}
		m := eval.ScoreInjection(results, c.hidden)
		fmt.Fprintf(&b, "min terms %d: %s\n", minTerms, m)
		if minTerms == application.DefaultMinTerms {
			def = m
			fmt.Fprintf(&b, "  irrelevant injections at the default gate:\n    %s\n", strings.Join(wrong, "\n    "))
		}
	}
	t.Logf("prompt injection, %d prompts:\n%s", len(prompts), b.String())
	if def.Stale > 0 {
		t.Errorf("stale items injected: %d", def.Stale)
	}
	if def.Precision() < floorInjectPrecision || float64(def.NoAnswerOK) < floorInjectSilence*float64(def.NoAnswer) {
		t.Errorf("injection regression at the default gate: %s", def)
	}
}
