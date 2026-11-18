package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"centrext/internal/application"
	"centrext/internal/domain"
	"centrext/internal/infrastructure/embeddings"
	"centrext/internal/infrastructure/keystore"
)

// embedderTimeout bounds one HTTP request; searches have their own, shorter
// budget (application.SemanticTimeout).
const embedderTimeout = 30 * time.Second

// loadEmbedder returns the configured embedder, or nil when embeddings are
// off. A missing key is reported once and search stays lexical (ADR-023).
func loadEmbedder(h string) domain.Embedder {
	cfg, ok, err := embeddings.LoadConfig(h)
	if err != nil {
		fmt.Fprintln(os.Stderr, "centrext: embeddings disabled:", err)
		return nil
	}
	if !ok {
		return nil
	}
	key, _, err := keystore.LoadEmbeddingsKey(h, embeddings.KeyEnv(cfg.Provider))
	if err != nil {
		fmt.Fprintln(os.Stderr, "centrext: embeddings disabled:", err)
		return nil
	}
	return embeddings.New(cfg, key, embedderTimeout)
}

func (a *app) cmdEmbeddings(ctx context.Context, h string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: centrext embeddings enable|key|status|sync|disable")
	}
	switch args[0] {
	case "enable":
		fs := newFlags("embeddings enable")
		provider := fs.String("provider", embeddings.OpenAI, "openai, voyage or openai-compatible")
		model := fs.String("model", "", "model name (default depends on the provider)")
		baseURL := fs.String("base-url", "", "API base URL (openai-compatible)")
		if _, err := parse(fs, args[1:]); err != nil {
			return err
		}
		cfg, err := embeddings.Config{Provider: *provider, Model: *model, BaseURL: *baseURL}.Defaults()
		if err != nil {
			return err
		}
		if err := embeddings.SaveConfig(h, cfg); err != nil {
			return err
		}
		fmt.Printf("Embeddings enabled: %s at %s\n", cfg.Model, cfg.BaseURL)
		if _, src, err := keystore.LoadEmbeddingsKey(h, embeddings.KeyEnv(cfg.Provider)); err != nil {
			fmt.Printf("Next: store the API key with `centrext embeddings key`, or set %s.\n", strings.Join(embeddings.KeyEnv(cfg.Provider), " or "))
		} else {
			fmt.Println("API key found in", src+".")
		}
		fmt.Println("Only namespaces whose policy allows content to leave this device are embedded;")
		fmt.Println("local_only namespaces (the default) stay lexical. Opt one in with `centrext namespace policy NS local_primary`.")
		return nil
	case "key":
		key, err := readSecret("API key: ")
		if err != nil {
			return err
		}
		if key == "" {
			return errors.New("no key given")
		}
		src, err := keystore.StoreEmbeddingsKey(h, key)
		if err != nil {
			return fmt.Errorf("store the key: %w", err)
		}
		fmt.Println("Stored the embeddings API key in the " + string(src) + ".")
		if src == keystore.FromFile {
			fmt.Println("No usable OS keychain, so it's in a file readable only by you (ADR-026).")
		}
		return nil
	case "status", "sync":
		emb := loadEmbedder(h)
		if emb == nil {
			if _, ok, _ := embeddings.LoadConfig(h); !ok {
				fmt.Println("Embeddings are off; search is lexical. Enable with `centrext embeddings enable`.")
				return nil
			}
			return keystore.ErrNoEmbeddingsKey
		}
		svc := application.New(a.store, application.WithEmbedder(emb))
		st, err := svc.EmbeddingStatus(ctx, a.user, args[0] == "sync")
		if err != nil {
			return err
		}
		fmt.Println("Model:", st.Model)
		fmt.Printf("Embedded: %d of %d active items in %d namespace(s)\n", st.Embedded, st.Active, len(st.Eligible))
		var names []string
		for _, n := range st.LocalOnly {
			names = append(names, n.Name)
		}
		if len(names) > 0 {
			fmt.Printf("Not embedded (local_only): %s\n", strings.Join(names, ", "))
		}
		if args[0] == "status" && st.Embedded < st.Active {
			fmt.Println("Missing vectors are computed on the next search, or now with `centrext embeddings sync`.")
		}
		return nil
	case "disable":
		if err := embeddings.RemoveConfig(h); err != nil {
			return err
		}
		if err := keystore.DeleteEmbeddingsKey(h); err != nil {
			fmt.Fprintln(os.Stderr, "centrext: couldn't remove the key from the keychain:", err)
		}
		fmt.Println("Embeddings disabled; search is lexical. Stored vectors are kept and reused if you enable the same model again.")
		return nil
	}
	return fmt.Errorf("unknown embeddings command %q", args[0])
}

// readSecret reads one line without echo on a terminal, or from a pipe.
func readSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
