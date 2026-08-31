package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfilePaths(t *testing.T) {
	t.Cleanup(func() { SetProfile("default") })

	SetProfile("")
	if got := Profile(); got != "default" {
		t.Fatalf("empty profile = %q, want default", got)
	}
	if !strings.HasSuffix(AuthFile(), filepath.Join("profiles", "default", "auth.json")) {
		t.Fatalf("default AuthFile = %q", AuthFile())
	}

	SetProfile("work")
	if !strings.HasSuffix(AuthFile(), filepath.Join("profiles", "work", "auth.json")) {
		t.Fatalf("work AuthFile = %q", AuthFile())
	}
}

func TestMigrateLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	base := filepath.Join(home, ".config", "colab-tui")
	if err := os.MkdirAll(filepath.Join(base, "pids"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"refresh_token":"x"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "sessions.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	if err := MigrateLegacy(); err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}

	defaultDir := filepath.Join(base, "profiles", "default")
	for _, name := range []string{"auth.json", "sessions.json", "pids"} {
		if _, err := os.Stat(filepath.Join(defaultDir, name)); err != nil {
			t.Errorf("migrated %s missing: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(base, name)); !os.IsNotExist(err) {
			t.Errorf("legacy %s still present", name)
		}
	}

	// Idempotent + non-clobbering: a second run with a stray legacy file left over
	// must not overwrite the already-migrated default profile.
	if err := os.WriteFile(filepath.Join(base, "auth.json"), []byte(`{"refresh_token":"new"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := MigrateLegacy(); err != nil {
		t.Fatalf("MigrateLegacy (2nd): %v", err)
	}
	data, err := os.ReadFile(filepath.Join(defaultDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"x"`) {
		t.Errorf("default auth.json was clobbered: %s", data)
	}
}

func TestMigrateLegacyNoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := MigrateLegacy(); err != nil {
		t.Fatalf("MigrateLegacy with nothing to migrate: %v", err)
	}
}

func TestOAuth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COLAB_CLIENT_ID", "")
	t.Setenv("COLAB_CLIENT_SECRET", "")

	// Nothing configured: an error that names the file to create.
	if _, err := OAuth(); err == nil {
		t.Fatal("OAuth with no credentials: want error")
	} else if !strings.Contains(err.Error(), OAuthFile()) {
		t.Errorf("error does not name %s: %v", OAuthFile(), err)
	}

	// File at the config root, shared by all profiles.
	base := filepath.Join(home, ".config", "colab-tui")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "oauth.json"),
		[]byte(`{"client_id":"file-id","client_secret":"file-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := OAuth()
	if err != nil {
		t.Fatalf("OAuth from file: %v", err)
	}
	if got.ClientID != "file-id" || got.ClientSecret != "file-secret" {
		t.Errorf("from file = %+v", got)
	}

	// Env wins over the file.
	t.Setenv("COLAB_CLIENT_ID", "env-id")
	t.Setenv("COLAB_CLIENT_SECRET", "env-secret")
	got, err = OAuth()
	if err != nil {
		t.Fatalf("OAuth from env: %v", err)
	}
	if got.ClientID != "env-id" || got.ClientSecret != "env-secret" {
		t.Errorf("from env = %+v", got)
	}
}
