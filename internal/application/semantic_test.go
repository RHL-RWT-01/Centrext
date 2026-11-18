package application_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
)

// conceptEmbedder maps words to concept dimensions, so paraphrases that share
// no words still get close vectors. It records every text it was sent.
type conceptEmbedder struct {
	mu   sync.Mutex
	sent []string
	fail error
}

var concepts = [][]string{
	{"database", "postgresql", "postgres", "persist", "stored", "storage", "records"},
	{"queue", "rabbitmq", "nats", "messages", "broker"},
	{"payments", "stripe", "checkout", "billing", "charge"},
	{"errors", "panic", "panics", "failures"},
}

func (e *conceptEmbedder) Model() string { return "fake/concepts" }

func (e *conceptEmbedder) Embed(_ context.Context, texts []string, _ domain.EmbedKind) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail != nil {
		return nil, e.fail
	}
	e.sent = append(e.sent, texts...)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(concepts)+1)
		v[len(concepts)] = 0.1 // never all zeros
		for _, w := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool { return r < 'a' || r > 'z' }) {
			for d, ws := range concepts {
				for _, c := range ws {
					if w == c {
						v[d]++
					}
				}
			}
		}
		var n float32
		for _, x := range v {
			n += x * x
		}
		for j := range v {
			v[j] /= sqrt32(n)
		}
		out[i] = v
	}
	return out, nil
}

func sqrt32(x float32) float32 {
	z := x
	for range 30 {
		z = (z + x/z) / 2
	}
	return z
}

func (e *conceptEmbedder) sentContaining(s string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.sent {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

func semanticFixture(t *testing.T, emb *conceptEmbedder, warn func(string)) *fixture {
	f := newFixture(t)
	f.svc = application.New(f.store, application.WithEmbedder(emb), application.WithWarn(warn),
		application.WithClock(func() time.Time { f.now = f.now.Add(time.Millisecond); return f.now }))
	return f
}

func (f *fixture) sharedNamespace(name string) domain.Namespace {
	f.t.Helper()
	ns, err := f.svc.CreateNamespace(f.ctx, f.user, application.NewNamespace{Name: name, StorageMode: domain.StorageLocalPrimary})
	if err != nil {
		f.t.Fatal(err)
	}
	return ns
}

func TestSemanticSearchFindsParaphrasesAndNeverSendsLocalOnly(t *testing.T) {
	emb := &conceptEmbedder{}
	f := semanticFixture(t, emb, nil)
	shop := f.sharedNamespace("shop")
	a := f.agent("cursor")
	f.propose(a, application.ProposeInput{NamespaceID: shop.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeDecision, Title: "Orders in PostgreSQL", Body: "Orders live in PostgreSQL behind an outbox."})
	f.propose(a, application.ProposeInput{NamespaceID: shop.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeDecision, Title: "Stripe", Body: "Stripe hosted checkout."})
	// Personal is local_only: never embedded, still found lexically.
	f.propose(a, application.ProposeInput{Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeFact, Title: "Private diary", Body: "My postgres password rotation day is Monday."})

	res, err := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "where are records persisted"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Head.Title != "Orders in PostgreSQL" {
		t.Fatalf("paraphrase should find the decision only: %+v", titles(res))
	}
	if emb.sentContaining("Private diary") || emb.sentContaining("diary") {
		t.Fatal("local_only content was sent to the embeddings API")
	}
	if !emb.sentContaining("Stripe hosted checkout") {
		t.Fatal("eligible content should be indexed")
	}
	if res, _ := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "diary"}); len(res.Items) != 1 {
		t.Fatal("local_only content must stay searchable lexically")
	}

	st, err := f.svc.EmbeddingStatus(f.ctx, f.user, false)
	if err != nil || st.Active != 2 || st.Embedded != 2 || len(st.LocalOnly) == 0 {
		t.Fatalf("status %+v %v", st, err)
	}
}

func TestSemanticSearchRespectsAccess(t *testing.T) {
	emb := &conceptEmbedder{}
	f := semanticFixture(t, emb, nil)
	shop, other := f.sharedNamespace("shop"), f.sharedNamespace("other")
	f.propose(f.user, application.ProposeInput{NamespaceID: other.ID, Kind: domain.KindCreate, Origin: domain.OriginUser,
		Type: domain.TypeDecision, Title: "Billing DB", Body: "Billing records persist in PostgreSQL."})
	a := f.agent("cursor", shop.ID)
	if _, err := f.svc.EmbeddingStatus(f.ctx, f.user, true); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "where are records persisted"})
	if err != nil || len(res.Items) != 0 {
		t.Fatalf("an agent must never get semantic matches from namespaces it can't read: %+v %v", titles(res), err)
	}
}

func TestSecretQueriesAreNotEmbedded(t *testing.T) {
	emb := &conceptEmbedder{}
	f := semanticFixture(t, emb, nil)
	f.sharedNamespace("shop")
	a := f.agent("cursor")
	q := "why does key AKIAIOSFODNN7EXAMPLE fail"
	if _, err := f.svc.Search(f.ctx, a, application.SearchQuery{Query: q}); err != nil {
		t.Fatal(err)
	}
	if emb.sentContaining("AKIA") {
		t.Fatal("a query containing a secret was sent to the embeddings API")
	}
}

func TestSemanticFailureFallsBackToLexical(t *testing.T) {
	emb := &conceptEmbedder{fail: errors.New("503 overloaded")}
	var warnings []string
	f := semanticFixture(t, emb, func(m string) { warnings = append(warnings, m) })
	shop := f.sharedNamespace("shop")
	a := f.agent("cursor")
	f.propose(a, application.ProposeInput{NamespaceID: shop.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeDecision, Title: "Orders in PostgreSQL", Body: "Orders live in PostgreSQL."})
	res, err := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "postgresql orders"})
	if err != nil || len(res.Items) != 1 {
		t.Fatalf("lexical fallback: %+v %v", titles(res), err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "503") {
		t.Fatalf("warnings: %v", warnings)
	}
}

func TestRevisionsAreReembedded(t *testing.T) {
	emb := &conceptEmbedder{}
	f := semanticFixture(t, emb, nil)
	shop := f.sharedNamespace("shop")
	a := f.agent("cursor")
	c := f.propose(a, application.ProposeInput{NamespaceID: shop.ID, Kind: domain.KindCreate, Origin: domain.OriginExplicit,
		Type: domain.TypeFact, Title: "Messaging", Body: "We use RabbitMQ."}).Context.Context
	if _, err := f.svc.EmbeddingStatus(f.ctx, f.user, true); err != nil {
		t.Fatal(err)
	}
	f.propose(a, application.ProposeInput{NamespaceID: shop.ID, Kind: domain.KindRevise, Origin: domain.OriginExplicit,
		TargetContextID: c.ID, BaseVersion: 1, Title: "Storage", Body: "Everything persists in PostgreSQL."})
	st, _ := f.svc.EmbeddingStatus(f.ctx, f.user, false)
	if st.Embedded != 0 || st.Active != 1 {
		t.Fatalf("revised item should need a new vector: %+v", st)
	}
	res, _ := f.svc.Search(f.ctx, a, application.SearchQuery{Query: "where are records stored"})
	if len(res.Items) != 1 || !emb.sentContaining("Everything persists") {
		t.Fatalf("revision should be re-embedded and found: %+v", titles(res))
	}
}

func titles(r application.SearchResult) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, it.Head.Title)
	}
	return out
}
