// Package config holds protocol constants and path helpers for the Colab CLI.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ColabDomain serves the tunnel (/tun/m/*) and XSRF calls; ColabGapiDomain
	// serves the newer v1/* API (assignments, proxy-token, user-info). The VS
	// Code extension (v0.8.1) splits traffic across these two hosts.
	ColabDomain     = "https://colab.research.google.com"
	ColabGapiDomain = "https://colab.pa.googleapis.com"

	OAuthScope = "profile email https://www.googleapis.com/auth/colaboratory"
	TokenURL   = "https://oauth2.googleapis.com/token"
	AuthURL    = "https://accounts.google.com/o/oauth2/v2/auth"

	TunBase           = "/tun/m"
	KeepaliveInterval = 300 // seconds — HTTP /keep-alive ping + token-refresh cadence
	HeartbeatInterval = 90  // seconds — WS ping on the held kernel connection

	// ExtVersion is reported via X-Colab-VS-Code-Extension-Version; bump it to
	// track the upstream extension the protocol is reverse-engineered from.
	ExtVersion = "0.8.1"
	AppName    = "Visual Studio Code"

	HProxyToken = "X-Colab-Runtime-Proxy-Token"
	HXsrf       = "X-Goog-Colab-Token"
	HAgent      = "X-Colab-Client-Agent"
	HAgentVal   = "vscode"
	HTunnel     = "X-Colab-Tunnel"
	HTunnelVal  = "Google"
	HAppName    = "X-Colab-VS-Code-App-Name"
	HExtVersion = "X-Colab-VS-Code-Extension-Version"
)

// activeProfile selects which on-disk profile (Google account) the CLI operates
// on. Each profile is an isolated directory tree, so different accounts run in
// parallel without sharing tokens, sessions, or keepalive daemons. Set via
// SetProfile (from the --profile flag or COLAB_PROFILE env) before any path
// helper is called; defaults to "default".
var activeProfile = "default"

// SetProfile selects the active profile. An empty name resets to "default".
func SetProfile(name string) {
	if name == "" {
		name = "default"
	}
	activeProfile = name
}

// Profile returns the active profile name.
func Profile() string { return activeProfile }

// baseDir is the root config tree shared by all profiles.
func baseDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "colab-tui")
}

// ProfilesDir holds one subdirectory per profile.
func ProfilesDir() string { return filepath.Join(baseDir(), "profiles") }

// activeFile persists the user-selected default profile (set via
// `colab profile use`). It sits at the config root, outside any profile dir.
func activeFile() string { return filepath.Join(baseDir(), "active") }

// ReadActiveProfile returns the persisted default profile name, or "" if none
// has been set. Used as a precedence step below the --profile flag and env.
func ReadActiveProfile() string {
	data, err := os.ReadFile(activeFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// WriteActiveProfile persists name as the default profile for future commands.
func WriteActiveProfile(name string) error {
	if err := os.MkdirAll(baseDir(), 0700); err != nil {
		return err
	}
	return os.WriteFile(activeFile(), []byte(name+"\n"), 0600)
}

// ConfigDir returns the active profile's config directory,
// e.g. ~/.config/colab-tui/profiles/<profile>.
func ConfigDir() string { return filepath.Join(ProfilesDir(), activeProfile) }

func AuthFile() string     { return filepath.Join(ConfigDir(), "auth.json") }
func SessionsFile() string { return filepath.Join(ConfigDir(), "sessions.json") }
func PidDir() string       { return filepath.Join(ConfigDir(), "pids") }

// OAuthFile holds the Google OAuth client credentials. It sits at the config
// root, outside any profile dir — the credentials identify the client (this
// tool), not the account, so every profile shares them.
func OAuthFile() string { return filepath.Join(baseDir(), "oauth.json") }

// OAuthCreds are the Colab OAuth "installed app" credentials. They belong to
// the Colab VS Code extension, which injects them at build time; this tool
// speaks that extension's protocol, so it needs the same pair.
type OAuthCreds struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// OAuth resolves the credentials: $COLAB_CLIENT_ID / $COLAB_CLIENT_SECRET if
// both are set, else OAuthFile(). They are deliberately not compiled in — a
// published client secret gets auto-revoked by Google.
func OAuth() (OAuthCreds, error) {
	id, secret := os.Getenv("COLAB_CLIENT_ID"), os.Getenv("COLAB_CLIENT_SECRET")
	if id != "" && secret != "" {
		return OAuthCreds{ClientID: id, ClientSecret: secret}, nil
	}

	data, err := os.ReadFile(OAuthFile())
	if err != nil {
		return OAuthCreds{}, missingOAuthError()
	}
	var c OAuthCreds
	if err := json.Unmarshal(data, &c); err != nil {
		return OAuthCreds{}, fmt.Errorf("config: parse %s: %w", OAuthFile(), err)
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		return OAuthCreds{}, fmt.Errorf("config: %s is missing client_id or client_secret", OAuthFile())
	}
	return c, nil
}

func missingOAuthError() error {
	return fmt.Errorf(`config: no Google OAuth credentials.

Create %s (mode 0600):

    {
      "client_id": "<id>.apps.googleusercontent.com",
      "client_secret": "<secret>"
    }

or set $COLAB_CLIENT_ID and $COLAB_CLIENT_SECRET.

The values are the Colab VS Code extension's installed-app credentials. The
extension injects them at build time, so they are not in its source: extract
them from the strings of out/extension.js in an installed .vsix. They are not
shipped here because a client secret published to a git host is auto-revoked.`, OAuthFile())
}

// MigrateLegacy moves a pre-multi-profile single-account config
// (~/.config/colab-tui/{auth.json,sessions.json,pids}) into the "default"
// profile directory. It is a no-op if there is nothing to migrate or the
// default profile already exists. Safe to call on every startup.
func MigrateLegacy() error {
	legacyAuth := filepath.Join(baseDir(), "auth.json")
	if _, err := os.Stat(legacyAuth); err != nil {
		return nil // nothing to migrate
	}
	defaultDir := filepath.Join(ProfilesDir(), "default")
	if _, err := os.Stat(filepath.Join(defaultDir, "auth.json")); err == nil {
		return nil // don't clobber an existing default profile
	}
	if err := os.MkdirAll(defaultDir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"auth.json", "sessions.json", "pids"} {
		src := filepath.Join(baseDir(), name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(defaultDir, name)); err != nil {
			return err
		}
	}
	return nil
}
