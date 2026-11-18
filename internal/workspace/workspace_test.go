package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Org/Repo.git":                "github.com/org/repo",
		"https://github.com/Org/Repo":                "github.com/org/repo",
		"https://github.com/org/repo.git/":           "github.com/org/repo",
		"https://user:pass@gitlab.example.com/a/b/c": "gitlab.example.com/a/b/c",
		"ssh://git@github.com:22/org/repo.git":       "github.com/org/repo",
		"git@bitbucket.org:team/project":             "bitbucket.org/team/project",
		"/srv/git/repo.git":                          "",
		"../other":                                   "",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func mkRepo(t *testing.T, dir, config string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveGitRemoteFromSubfolder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "myrepo")
	mkRepo(t, root, "[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:Acme/Shop.git\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n[remote \"upstream\"]\n\turl = https://github.com/other/shop\n")
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	id, err := Resolve(sub)
	if err != nil {
		t.Fatal(err)
	}
	if id.Kind != KindGitRemote || id.Value != "git:github.com/acme/shop" || id.Name != "myrepo" {
		t.Fatalf("got %+v", id)
	}
}

func TestResolveSingleNonOriginRemote(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "[remote \"github\"]\n\turl = https://github.com/acme/one\n")
	id, _ := Resolve(root)
	if id.Value != "git:github.com/acme/one" {
		t.Fatalf("got %+v", id)
	}
}

func TestResolveAmbiguousRemotesFallsBackToRepoRoot(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "[remote \"a\"]\n\turl = https://github.com/acme/a\n[remote \"b\"]\n\turl = https://github.com/acme/b\n")
	id, _ := Resolve(root)
	if id.Kind != KindRepoRoot || !id.Ambiguous || len(id.RemoteNames) != 2 {
		t.Fatalf("got %+v", id)
	}
}

func TestResolveNoRemote(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "[core]\n")
	id, _ := Resolve(root)
	if id.Kind != KindRepoRoot || id.Value == "" {
		t.Fatalf("got %+v", id)
	}
}

func TestWorktreeMatchesMainCheckout(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "main")
	mkRepo(t, main, "[core]\n") // no remote: identity is the common dir
	wtGit := filepath.Join(main, ".git", "worktrees", "feat")
	os.MkdirAll(wtGit, 0o755)
	os.WriteFile(filepath.Join(wtGit, "commondir"), []byte("../..\n"), 0o644)
	wt := filepath.Join(base, "feat")
	os.MkdirAll(wt, 0o755)
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGit+"\n"), 0o644)

	a, _ := Resolve(main)
	b, _ := Resolve(wt)
	if a.Value != b.Value || b.Name != "main" {
		t.Fatalf("main %+v, worktree %+v", a, b)
	}
}

func TestMarkerWins(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "[remote \"origin\"]\n\turl = https://github.com/acme/x\n")
	if err := WriteMarker(root, "ns_123"); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "pkg")
	os.MkdirAll(sub, 0o755)
	id, _ := Resolve(sub)
	if id.Kind != KindMarker || id.Value != "ns_123" {
		t.Fatalf("got %+v", id)
	}
}

func TestNotARepo(t *testing.T) {
	id, _ := Resolve(t.TempDir())
	if id.Kind != KindNone {
		t.Fatalf("got %+v", id)
	}
	if id, _ := Resolve(""); id.Kind != KindNone {
		t.Fatal("empty dir should be none")
	}
}
