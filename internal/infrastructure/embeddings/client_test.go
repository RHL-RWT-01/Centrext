package embeddings_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"centrext/internal/domain"
	"centrext/internal/infrastructure/embeddings"
)

func TestClientBatchesNormalizesAndSendsProviderFields(t *testing.T) {
	var calls []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer k-test" {
			http.Error(w, `{"error":{"message":"bad request"}}`, 400)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body)
		in := body["input"].([]any)
		var data []map[string]any
		for i := len(in) - 1; i >= 0; i-- { // out of order on purpose
			data = append(data, map[string]any{"index": i, "embedding": []float64{3, 4, float64(i)}})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	cfg, err := embeddings.Config{Provider: embeddings.Voyage, BaseURL: "http://127.0.0.1" + strings.TrimPrefix(srv.URL, "http://127.0.0.1") + "/v1"}.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	c := embeddings.New(cfg, "k-test", 5*time.Second)
	texts := make([]string, embeddings.BatchSize+3)
	for i := range texts {
		texts[i] = fmt.Sprint("text ", i)
	}
	vs, err := c.Embed(context.Background(), texts, domain.EmbedQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(vs) != len(texts) {
		t.Fatalf("calls=%d vectors=%d", len(calls), len(vs))
	}
	if calls[0]["input_type"] != "query" || calls[0]["model"] != "voyage-3.5-lite" {
		t.Fatalf("request: %v", calls[0])
	}
	if v := vs[0]; math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Fatalf("not normalized or out of order: %v", v)
	}
	if c.Model() != "voyage/voyage-3.5-lite" {
		t.Fatal(c.Model())
	}
}

func TestClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer srv.Close()
	cfg, _ := embeddings.Config{Provider: embeddings.OpenAICompatible, BaseURL: srv.URL, Model: "m"}.Defaults()
	_, err := embeddings.New(cfg, "secret-key-value", time.Second).Embed(context.Background(), []string{"x"}, domain.EmbedDocument)
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key") || strings.Contains(err.Error(), "secret-key-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	if c, err := (embeddings.Config{Provider: "openai"}).Defaults(); err != nil || c.Model != "text-embedding-3-small" || c.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := (embeddings.Config{Provider: "openai", BaseURL: "http://example.com"}).Defaults(); err == nil {
		t.Fatal("plain http to a remote host must be refused")
	}
	if _, err := (embeddings.Config{Provider: "openai-compatible"}).Defaults(); err == nil {
		t.Fatal("openai-compatible needs a base URL and model")
	}
}

func TestCosine(t *testing.T) {
	if embeddings.Cosine([]float32{1, 0}, []float32{1, 0}) != 1 || embeddings.Cosine([]float32{1}, []float32{1, 0}) != 0 {
		t.Fatal("cosine")
	}
}
