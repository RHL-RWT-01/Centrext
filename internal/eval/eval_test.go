package eval_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/eval"
	"centrext/internal/infrastructure/sqlite"
)

// Floors are the baseline measured when the harness was added (lexical FTS5
// x trust, relative score cutoff). The test fails on a regression below them, not for missing the
// section 7 targets (P@5 >= 80%, R@5 >= 75%), which 0.3 has to earn.
// Raise them when ranking improves.
const (
	floorPrecision = 0.82 // measured 0.83 with MinRelativeScore 0.4
	floorRecall    = 0.89 // measured 0.90
)

// TestRetrievalEval builds the corpus through the application layer (so
// supersede, delete and expiry use production paths), runs every query as an
// agent, and reports the metrics. Run with -v to see the table.
func TestRetrievalEval(t *testing.T) {
	ds, err := eval.Load(filepath.Join("testdata", "retrieval.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "eval.db"), bytes.Repeat([]byte{9}, sqlite.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	now := base
	svc := application.New(store, application.WithClock(func() time.Time { return now }))
	if _, err := svc.Init(ctx, "eval"); err != nil {
		t.Fatal(err)
	}
	user, _ := svc.LocalUser(ctx)
	nsIDs := map[string]string{}
	personal, _ := svc.FindNamespace(ctx, user, application.PersonalNamespace)
	nsIDs[application.PersonalNamespace] = personal.ID
	for _, name := range ds.Namespaces {
		ns, err := svc.CreateNamespace(ctx, user, application.NewNamespace{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		nsIDs[name] = ns.ID
	}
	agent := func(name string) domain.Actor {
		_, tok, err := svc.RegisterAgent(ctx, user, application.RegisterAgentInput{Name: name, Client: name, AllNamespaces: true})
		if err != nil {
			t.Fatal(err)
		}
		a, _ := svc.Authenticate(ctx, tok)
		return a
	}
	writer, reader := agent("writer"), agent("reader")

	ids := map[string]string{}  // key -> context id
	keys := map[string]string{} // context id -> key
	hidden := map[string]bool{}
	for _, c := range ds.Contexts {
		now = base.Add(-time.Duration(c.AgeDays) * 24 * time.Hour)
		in := application.ProposeInput{NamespaceID: nsIDs[c.NS], Kind: domain.KindCreate,
			Type: domain.ContextType(c.Type), Title: c.Title, Body: c.Body}
		actor := user
		switch c.Trust {
		case "user_verified":
			in.Origin = domain.OriginUser
		case "session_confirmed":
			in.Origin, actor = domain.OriginExplicit, writer
		case "agent_reported":
			in.Origin, actor = domain.OriginSilent, writer
		default:
			t.Fatalf("%s: unsupported trust %q", c.Key, c.Trust)
		}
		if c.Supersedes != "" {
			in.Kind, in.TargetContextID, in.BaseVersion, in.Origin, actor = domain.KindSupersede, ids[c.Supersedes], 1, domain.OriginUser, user
		}
		res, err := svc.Propose(ctx, actor, in)
		if err != nil || res.Context == nil {
			t.Fatalf("%s: %v (%+v)", c.Key, err, res.Proposal)
		}
		id := res.Context.Context.ID
		ids[c.Key], keys[id] = id, c.Key
		if c.Deleted {
			if err := svc.DeleteContext(ctx, user, id, res.Context.Context.CurrentVersion); err != nil {
				t.Fatal(err)
			}
		}
		if c.Hidden {
			hidden[c.Key] = true
		}
	}
	now = base

	var results []eval.Result
	var misses []string
	for _, q := range ds.Queries {
		res, err := svc.Search(ctx, reader, application.SearchQuery{Query: q.Q, NamespaceID: nsIDs[q.NS], Limit: eval.K, TokenBudget: 1 << 20})
		if err != nil {
			t.Fatalf("%q: %v", q.Q, err)
		}
		r := eval.Result{Query: q}
		for _, it := range res.Items {
			r.Returned = append(r.Returned, keys[it.Context.ID])
		}
		results = append(results, r)
		if len(q.Relevant) > 0 && (len(r.Returned) == 0 || !contains(q.Relevant, r.Returned[0])) {
			misses = append(misses, q.Cat+": "+q.Q+" -> ["+strings.Join(r.Returned, ", ")+"]")
		}
	}

	ms := eval.Score(results, hidden)
	all := ms["all"]
	t.Logf("retrieval eval: %d contexts, %d queries\n%s", len(ds.Contexts), len(ds.Queries), eval.Report(ms))
	t.Logf("top result not relevant (%d):\n  %s", len(misses), strings.Join(misses, "\n  "))
	if all.StaleQueries > 0 {
		t.Errorf("stale retrieval in %d queries; superseded, deleted and expired items must never be returned", all.StaleQueries)
	}
	if all.Precision < floorPrecision || all.Recall < floorRecall {
		t.Errorf("regression: P@5 %.2f (floor %.2f), R@5 %.2f (floor %.2f)", all.Precision, floorPrecision, all.Recall, floorRecall)
	}
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
