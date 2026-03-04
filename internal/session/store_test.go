package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := NewStore(path)
	s.Add(&Session{
		Label:    "train",
		Endpoint: "ep1",
		Variant:  "GPU",
	})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s2 := NewStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := s2.GetByLabel("train")
	if got == nil {
		t.Fatal("GetByLabel returned nil")
	}
	if got.Endpoint != "ep1" {
		t.Errorf("Endpoint: got %q, want %q", got.Endpoint, "ep1")
	}
	if got.Variant != "GPU" {
		t.Errorf("Variant: got %q, want %q", got.Variant, "GPU")
	}
}

func TestStoreGetDefault(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "sessions.json"))

	older := &Session{Label: "older", AssignedAt: time.Now().Add(-time.Hour)}
	newer := &Session{Label: "newer", AssignedAt: time.Now()}
	s.Add(older)
	s.Add(newer)

	got := s.GetDefault()
	if got == nil {
		t.Fatal("GetDefault returned nil")
	}
	if got.Label != "newer" {
		t.Errorf("GetDefault: got label %q, want %q", got.Label, "newer")
	}
}

func TestStoreRemove(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "sessions.json"))

	s.Add(&Session{Label: "gone"})
	s.Remove("gone")

	if got := s.GetByLabel("gone"); got != nil {
		t.Errorf("expected nil after Remove, got %+v", got)
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	s := NewStore(path)
	s.Add(&Session{Label: "perm-test"})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file permissions: got %04o, want 0600", perm)
	}
}
