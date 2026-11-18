// Package workspace works out which project a folder belongs to (ADR-017).
// It only reads files; it never runs git and never touches the database.
package workspace

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind says how a folder was identified.
type Kind string

const (
	KindMarker    Kind = "marker"     // a .centrext file names the namespace
	KindGitRemote Kind = "git_remote" // normalized remote URL
	KindRepoRoot  Kind = "repo_root"  // git repo with no usable remote
	KindNone      Kind = "none"       // not in a repo: personal namespace
)

// MarkerFile is the optional file that pins a folder to a namespace.
const MarkerFile = ".centrext"

// Identity is the result of resolving a folder.
type Identity struct {
	Kind        Kind
	Value       string // namespace id (marker), "git:host/path" or "repo:/abs/common/dir"
	Dir         string // folder the marker or repo was found in
	Name        string // suggested namespace name (repo folder name)
	Ambiguous   bool   // several remotes and none is origin
	RemoteNames []string
}

// Resolve identifies the project that dir belongs to.
func Resolve(dir string) (Identity, error) {
	if dir == "" {
		return Identity{Kind: KindNone}, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Identity{}, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return Identity{Kind: KindNone}, nil
	}

	// 1. A marker file anywhere above wins.
	for d := abs; ; d = filepath.Dir(d) {
		if id, ok := readMarker(filepath.Join(d, MarkerFile)); ok {
			return Identity{Kind: KindMarker, Value: id, Dir: d, Name: filepath.Base(d)}, nil
		}
		if filepath.Dir(d) == d {
			break
		}
	}

	// 2-3. The nearest git repo.
	for d := abs; ; d = filepath.Dir(d) {
		if common, ok := gitCommonDir(d); ok {
			return identifyRepo(d, common), nil
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	return Identity{Kind: KindNone}, nil
}

// readMarker reads `namespace = "ns_..."` from a marker file.
func readMarker(path string) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "namespace" {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// WriteMarker pins dir to a namespace.
func WriteMarker(dir, namespaceID string) error {
	body := "# Centrext: this folder's shared context namespace. Safe to commit (no secrets).\nnamespace = \"" + namespaceID + "\"\n"
	return os.WriteFile(filepath.Join(dir, MarkerFile), []byte(body), 0o644)
}

// gitCommonDir returns the repository's shared git directory if dir is a
// repo root. Worktrees share it, so they map to the same project.
func gitCommonDir(dir string) (string, bool) {
	p := filepath.Join(dir, ".git")
	fi, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	gitDir := p
	if !fi.IsDir() {
		// Worktree or submodule: ".git" is a file with "gitdir: <path>".
		b, err := os.ReadFile(p)
		if err != nil {
			return "", false
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir:") {
			return "", false
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
	}
	common := gitDir
	if b, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = c
	}
	if abs, err := filepath.EvalSymlinks(common); err == nil {
		common = abs
	}
	return filepath.Clean(common), true
}

var remoteHeader = regexp.MustCompile(`^\[remote\s+"([^"]+)"\]$`)

// remotes parses [remote "name"] url = ... from a git config file.
func remotes(configPath string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(configPath)
	if err != nil {
		return out
	}
	defer f.Close()
	current := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			current = ""
			if m := remoteHeader.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			continue
		}
		if current == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == "url" {
			if _, seen := out[current]; !seen {
				out[current] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

func identifyRepo(root, common string) Identity {
	name := filepath.Base(root)
	// For a worktree, name the project after the main checkout.
	if filepath.Base(common) == ".git" {
		name = filepath.Base(filepath.Dir(common))
	}
	rs := remotes(filepath.Join(common, "config"))
	url := rs["origin"]
	if url == "" && len(rs) == 1 {
		for _, u := range rs {
			url = u
		}
	}
	if url != "" {
		if norm := NormalizeRemote(url); norm != "" {
			return Identity{Kind: KindGitRemote, Value: "git:" + norm, Dir: root, Name: name}
		}
	}
	id := Identity{Kind: KindRepoRoot, Value: "repo:" + common, Dir: root, Name: name}
	if len(rs) > 1 {
		id.Ambiguous = true
		for n := range rs {
			id.RemoteNames = append(id.RemoteNames, n)
		}
	}
	return id
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// NormalizeRemote turns any git remote URL into host/path, e.g.
// git@github.com:Org/Repo.git and https://github.com/Org/Repo both become
// github.com/org/repo. Credentials and ports are dropped.
func NormalizeRemote(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}
	var host, path string
	if i := strings.Index(u, "://"); i >= 0 {
		rest := u[i+3:]
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path, _ = strings.Cut(rest, "/")
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	} else if m := scpLike.FindStringSubmatch(u); m != nil && !strings.HasPrefix(u, "/") && !strings.HasPrefix(u, ".") {
		host, path = m[1], m[2]
	} else {
		return "" // local path remote: not a stable identity
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" {
		return ""
	}
	return strings.ToLower(host + "/" + path)
}

// RepoRoot returns the nearest folder at or above dir that is a git repo root, or "".
func RepoRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, ok := gitCommonDir(d); ok {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}
