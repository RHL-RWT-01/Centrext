package eval

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"centrext/internal/domain"
	"centrext/internal/fsutil"
)

// VectorCache is a domain.Embedder that replays recorded vectors, so the
// hybrid eval runs offline and deterministically. With a live embedder set,
// it records vectors it doesn't have yet (ADR-023).
type VectorCache struct {
	mu      sync.Mutex
	model   string
	vectors map[string][]float32 // key: sha256(kind, text)
	live    domain.Embedder
	misses  int
}

type cacheFile struct {
	Model   string            `json:"model"`
	Vectors map[string]string `json:"vectors"` // base64 of little-endian float32
}

// LoadVectorCache reads a cache file. live may be nil (replay only).
func LoadVectorCache(path string, live domain.Embedder) (*VectorCache, error) {
	c := &VectorCache{vectors: map[string][]float32{}, live: live}
	if live != nil {
		c.model = live.Model()
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && live != nil {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f cacheFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if live != nil && f.Model != live.Model() {
		return nil, fmt.Errorf("%s holds %s vectors, not %s", path, f.Model, live.Model())
	}
	c.model = f.Model
	for k, v := range f.Vectors {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(raw)%4 != 0 {
			return nil, fmt.Errorf("%s: malformed vector %s", path, k)
		}
		vec := make([]float32, len(raw)/4)
		for i := range vec {
			vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
		}
		c.vectors[k] = vec
	}
	return c, nil
}

func cacheKey(kind domain.EmbedKind, text string) string {
	h := sha256.Sum256([]byte(string(kind) + "\x00" + text))
	return hex.EncodeToString(h[:12])
}

// Model implements domain.Embedder.
func (c *VectorCache) Model() string { return c.model }

// Misses counts texts that had no recorded vector.
func (c *VectorCache) Misses() int { c.mu.Lock(); defer c.mu.Unlock(); return c.misses }

// Embed implements domain.Embedder.
func (c *VectorCache) Embed(ctx context.Context, texts []string, kind domain.EmbedKind) ([][]float32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]float32, len(texts))
	var missing []int
	for i, t := range texts {
		if v, ok := c.vectors[cacheKey(kind, t)]; ok {
			out[i] = v
		} else {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	c.misses += len(missing)
	if c.live == nil {
		return nil, fmt.Errorf("no recorded vector for %d text(s); record again (see internal/eval)", len(missing))
	}
	in := make([]string, len(missing))
	for j, i := range missing {
		in[j] = texts[i]
	}
	vs, err := c.live.Embed(ctx, in, kind)
	if err != nil {
		return nil, err
	}
	for j, i := range missing {
		out[i] = vs[j]
		c.vectors[cacheKey(kind, texts[i])] = vs[j]
	}
	return out, nil
}

// Save writes the cache. Output is deterministic (sorted keys).
func (c *VectorCache) Save(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := cacheFile{Model: c.model, Vectors: map[string]string{}}
	for k, vec := range c.vectors {
		raw := make([]byte, 4*len(vec))
		for i, x := range vec {
			binary.LittleEndian.PutUint32(raw[i*4:], math.Float32bits(x))
		}
		f.Vectors[k] = base64.StdEncoding.EncodeToString(raw)
	}
	b, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'), 0o644)
}
