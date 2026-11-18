package session_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"centrext/internal/session"
)

func TestDeliveredRoundTripAndPrune(t *testing.T) {
	home := t.TempDir()
	d := session.Load(home, "abc")
	if !d.Add("ctx_1", "ctx_2") || d.Add("ctx_1") {
		t.Fatal("Add should report only new ids")
	}
	old := filepath.Join(home, session.Dir, "old.json")
	os.MkdirAll(filepath.Dir(old), 0o700)
	os.WriteFile(old, []byte(`{"ids":[]}`), 0o600)
	past := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(old, past, past)
	if err := d.Save(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := session.Load(home, "abc").IDs; len(got) != 2 {
		t.Fatalf("ids = %v", got)
	}
	if len(session.Load(home, "other").IDs) != 0 {
		t.Fatal("sessions must not share records")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("records older than MaxAge should be removed")
	}
	if err := session.Load(home, "").Save(time.Now()); err != nil {
		t.Fatal(err)
	}
}
