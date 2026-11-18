package retrieval

import (
	"testing"

	"centrext/internal/domain"
)

func TestBuildMatchIsSafe(t *testing.T) {
	cases := map[string]string{
		"Which database should we use?":  `"database"*`,
		`postgres" OR title:* NEAR(x y)`: `"postgres"* OR "title"* OR "near"*`,
		"":                               "",
		"a ? !":                          "",
		"Postgres postgres outbox":       `"postgres"* OR "outbox"*`,
	}
	for in, want := range cases {
		if got := BuildMatch(in); got != want {
			t.Errorf("BuildMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRankPrefersTrustOnEqualMatch(t *testing.T) {
	hits := []domain.SearchHit{
		{Context: domain.Context{ID: "agent", HeadTrust: domain.TrustAgentReported}, Lexical: 2},
		{Context: domain.Context{ID: "user", HeadTrust: domain.TrustUserVerified}, Lexical: 2},
		{Context: domain.Context{ID: "weak", HeadTrust: domain.TrustUserVerified}, Lexical: 0.5},
	}
	r := Rank(hits)
	if r[0].Hit.Context.ID != "user" || r[1].Hit.Context.ID != "agent" || r[2].Hit.Context.ID != "weak" {
		t.Fatalf("order: %s %s %s", r[0].Hit.Context.ID, r[1].Hit.Context.ID, r[2].Hit.Context.ID)
	}
	if r[0].Score != 1 {
		t.Fatalf("top score = %v", r[0].Score)
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("abcdefgh") != 2 || EstimateTokens("") != 0 || EstimateTokens("abcde") != 2 {
		t.Fatal("token estimate off")
	}
}
