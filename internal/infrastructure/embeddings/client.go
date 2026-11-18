// Package embeddings calls a hosted embeddings API (ADR-023). It speaks the
// OpenAI-compatible /embeddings protocol, which OpenAI and Voyage both serve.
// What may be sent is decided by the application layer, not here.
package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"centrext/internal/domain"
)

// Providers with built-in defaults. "openai-compatible" needs a base URL and model.
const (
	OpenAI           = "openai"
	Voyage           = "voyage"
	OpenAICompatible = "openai-compatible"
)

// Config selects a provider and model. It holds no secret.
type Config struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
}

// Defaults fills in the provider's base URL and default model.
func (c Config) Defaults() (Config, error) {
	switch c.Provider {
	case OpenAI:
		c.BaseURL = or(c.BaseURL, "https://api.openai.com/v1")
		c.Model = or(c.Model, "text-embedding-3-small")
	case Voyage:
		c.BaseURL = or(c.BaseURL, "https://api.voyageai.com/v1")
		c.Model = or(c.Model, "voyage-3.5-lite")
	case OpenAICompatible:
		if c.BaseURL == "" || c.Model == "" {
			return c, errors.New("openai-compatible needs --base-url and --model")
		}
	default:
		return c, fmt.Errorf("unknown provider %q; use openai, voyage or openai-compatible", c.Provider)
	}
	if !strings.HasPrefix(c.BaseURL, "https://") && !strings.HasPrefix(c.BaseURL, "http://localhost") && !strings.HasPrefix(c.BaseURL, "http://127.0.0.1") {
		return c, errors.New("base URL must use https (plain http only for localhost)")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	return c, nil
}

func or(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// KeyEnv lists the environment variables a provider's key may come from, in order.
func KeyEnv(provider string) []string {
	switch provider {
	case OpenAI:
		return []string{"CENTREXT_EMBEDDINGS_KEY", "OPENAI_API_KEY"}
	case Voyage:
		return []string{"CENTREXT_EMBEDDINGS_KEY", "VOYAGE_API_KEY"}
	}
	return []string{"CENTREXT_EMBEDDINGS_KEY"}
}

// BatchSize is how many texts go in one request.
const BatchSize = 96

// maxChars keeps each text well under providers' per-input token limits.
const maxChars = 24000

// Client is a domain.Embedder backed by an HTTP API.
type Client struct {
	cfg  Config
	key  string
	http *http.Client
}

var _ domain.Embedder = (*Client)(nil)

// New returns a client. cfg must have been through Defaults.
func New(cfg Config, key string, timeout time.Duration) *Client {
	return &Client{cfg: cfg, key: key, http: &http.Client{Timeout: timeout}}
}

// Model implements domain.Embedder.
func (c *Client) Model() string { return c.cfg.Provider + "/" + c.cfg.Model }

type request struct {
	Model     string   `json:"model"`
	Input     []string `json:"input"`
	InputType string   `json:"input_type,omitempty"` // Voyage only
	Encoding  string   `json:"encoding_format,omitempty"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Detail string `json:"detail"` // Voyage errors
}

// Embed implements domain.Embedder. Vectors are normalized to unit length.
func (c *Client) Embed(ctx context.Context, texts []string, kind domain.EmbedKind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += BatchSize {
		end := min(start+BatchSize, len(texts))
		vs, err := c.embedBatch(ctx, texts[start:end], kind)
		if err != nil {
			return nil, err
		}
		out = append(out, vs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string, kind domain.EmbedKind) ([][]float32, error) {
	req := request{Model: c.cfg.Model, Input: make([]string, len(texts))}
	for i, t := range texts {
		if len(t) > maxChars {
			t = t[:maxChars]
		}
		if strings.TrimSpace(t) == "" {
			t = "-" // providers reject empty input
		}
		req.Input[i] = t
	}
	if c.cfg.Provider == Voyage {
		req.InputType = string(kind)
	} else {
		req.Encoding = "float"
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+c.key)
	hr.Header.Set("User-Agent", "centrext")
	resp, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var r response
	_ = json.Unmarshal(raw, &r)
	if resp.StatusCode != http.StatusOK {
		msg := resp.Status
		if r.Error != nil && r.Error.Message != "" {
			msg += ": " + r.Error.Message
		} else if r.Detail != "" {
			msg += ": " + r.Detail
		}
		return nil, fmt.Errorf("embeddings API: %s", msg)
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings API returned %d vectors for %d inputs", len(r.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil || len(d.Embedding) == 0 {
			return nil, errors.New("embeddings API returned a malformed vector list")
		}
		out[d.Index] = normalize(d.Embedding)
	}
	return out, nil
}

func normalize(v []float64) []float32 {
	var n float64
	for _, x := range v {
		n += x * x
	}
	n = math.Sqrt(n)
	out := make([]float32, len(v))
	if n == 0 {
		return out
	}
	for i, x := range v {
		out[i] = float32(x / n)
	}
	return out
}

// Cosine is the dot product of two unit vectors (0 if dimensions differ).
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
