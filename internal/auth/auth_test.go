package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	m := &Manager{RefreshToken: "test-rt"}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.RefreshToken != "test-rt" {
		t.Errorf("RefreshToken = %q, want %q", loaded.RefreshToken, "test-rt")
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	m := &Manager{RefreshToken: "test-rt"}
	if err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file permissions = %04o, want 0600", perm)
	}
}

func TestLoadMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load of missing file should not error, got: %v", err)
	}
	if m.RefreshToken != "" {
		t.Errorf("expected empty Manager, got RefreshToken=%q", m.RefreshToken)
	}
}
