package connect_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"centrext/internal/application"
	"centrext/internal/connect"
	"centrext/internal/workspace"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), "app")
	os.MkdirAll(dir, 0o755)
	git(t, dir, "init", "-q")
	git(t, dir, "remote", "add", "origin", "git@github.com:acme/app.git")
	return dir
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func find(checks []connect.Check, st connect.Status, substr string) *connect.Check {
	for i, c := range checks {
		if c.Status == st && strings.Contains(c.Area+" "+c.Message, substr) {
			return &checks[i]
		}
	}
	return nil
}

func fails(checks []connect.Check) []connect.Check {
	var out []connect.Check
	for _, c := range checks {
		if c.Status == connect.Fail {
			out = append(out, c)
		}
	}
	return out
}

// installBinary makes env.Binary a real file so command checks pass.
func (f *fixture) installBinary() {
	f.env.Binary = filepath.Join(f.t.TempDir(), "centrext")
	os.WriteFile(f.env.Binary, []byte("#!/bin/sh\n"), 0o755)
}

// claudeRegistered writes what `claude mcp add-json --scope user` would.
func (f *fixture) claudeRegistered() {
	writeFile(f.t, filepath.Join(f.env.UserHome, ".claude.json"),
		`{"numStartups":3,"mcpServers":{"centrext":{"type":"stdio","command":"`+f.env.Binary+`","args":["mcp","--client","claude-code"]}}}`)
}

func TestDoctorHealthyInstall(t *testing.T) {
	f := newFixture(t)
	f.installBinary()
	dir := gitRepo(t)
	f.connect(connect.Options{Client: connect.ClaudeCode})
	f.claudeRegistered()
	f.connect(connect.Options{Client: connect.ClaudeDesktop})
	f.connect(connect.Options{Client: connect.Cursor, Dir: dir})
	git(t, dir, "add", "-A") // .cursor/mcp.json and .gitignore get committed; the rules file is ignored

	checks := connect.Doctor(f.ctx, f.svc, f.user, f.env, dir)
	if fl := fails(checks); len(fl) > 0 {
		t.Fatalf("unexpected failures: %+v", fl)
	}
	for _, want := range []string{"claude-code", "claude-desktop", "cursor", "no tokens in files git tracks"} {
		if find(checks, connect.OK, want) == nil {
			t.Fatalf("missing ok check %q in %+v", want, checks)
		}
	}
	if c := find(checks, connect.Warn, ""); c != nil {
		t.Fatalf("unexpected warning: %+v", c)
	}
}

func TestDoctorFindsBrokenInstall(t *testing.T) {
	f := newFixture(t)
	f.installBinary()
	dir := gitRepo(t)
	claude := f.connect(connect.Options{Client: connect.ClaudeCode})
	f.claudeRegistered()
	f.connect(connect.Options{Client: connect.ClaudeDesktop})
	f.connect(connect.Options{Client: connect.Cursor, Dir: dir})

	delete(f.tokens, claude.AgentID)                                                    // token lost
	os.Remove(filepath.Join(f.env.UserHome, ".claude", "settings.json"))                // hook gone
	os.WriteFile(filepath.Join(dir, connect.RulesFile), []byte("hand edited\n"), 0o644) // stale rules
	git(t, dir, "add", "-f", filepath.ToSlash(connect.RulesFile))                       // and committed
	os.Remove(f.env.Binary)                                                             // binary moved away
	f.installBinary()                                                                   // doctor runs from the new path

	checks := connect.Doctor(f.ctx, f.svc, f.user, f.env, dir)
	for _, c := range []struct {
		st   connect.Status
		text string
		fix  string
	}{
		{connect.Fail, "no stored token for agent " + claude.AgentID, "centrext connect claude-code"},
		{connect.Fail, "no SessionStart hook", "centrext connect claude-code"},
		{connect.Fail, "claude_desktop_config.json runs", "centrext connect claude-desktop"},
		{connect.Warn, "rules file is out of date", "centrext connect cursor --dir"},
		{connect.Warn, "is committed", "rm --cached"},
		{connect.Fail, ".claude.json runs", "centrext connect claude-code"},
		{connect.Fail, "mcp.json runs", "centrext connect cursor --dir"},
	} {
		got := find(checks, c.st, c.text)
		if got == nil || !strings.Contains(got.Fix, c.fix) {
			t.Fatalf("want %v %q (fix %q); got %+v\nall: %+v", c.st, c.text, c.fix, got, checks)
		}
	}
}

func TestDoctorFindsTokensInTrackedFiles(t *testing.T) {
	f := newFixture(t)
	dir := gitRepo(t)
	_, active, _ := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "leaky", Client: "cursor", AllNamespaces: true})
	revokedAgent, revoked, _ := f.svc.RegisterAgent(f.ctx, f.user, application.RegisterAgentInput{Name: "old", Client: "cursor", AllNamespaces: true})
	f.svc.RevokeAgent(f.ctx, f.user, revokedAgent.ID, false)

	writeFile(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers":{"centrext":{"env":{"CENTREXT_TOKEN":"`+active+`"}}}}`)
	writeFile(t, filepath.Join(dir, "notes.md"), "old token "+revoked+"\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), active)
	git(t, dir, "add", ".mcp.json", "notes.md")

	checks := connect.Doctor(f.ctx, f.svc, f.user, f.env, filepath.Join(dir))
	leak := find(checks, connect.Fail, ".mcp.json (tracked by git) contains the active token")
	if leak == nil || !strings.Contains(leak.Fix, "centrext agent revoke agt_") {
		t.Fatalf("active token leak not reported: %+v", checks)
	}
	if find(checks, connect.Warn, "notes.md (tracked by git) contains a Centrext token, not an active one") == nil {
		t.Fatalf("revoked token not reported: %+v", checks)
	}
	for _, c := range checks {
		if strings.Contains(c.Message, "untracked.txt") || strings.Contains(c.Message+c.Fix, active) {
			t.Fatalf("doctor must only scan tracked files and never print tokens: %+v", c)
		}
	}
}

func TestDoctorChecksMarker(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	workspace.WriteMarker(dir, "ns_missing")
	checks := connect.Doctor(f.ctx, f.svc, f.user, f.env, dir)
	if find(checks, connect.Fail, "names namespace ns_missing") == nil {
		t.Fatalf("bad marker not reported: %+v", checks)
	}
	if find(checks, connect.Warn, "no clients are connected") == nil {
		t.Fatal("should say nothing is connected")
	}
}
