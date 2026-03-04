// Package config holds protocol constants and path helpers for the Colab CLI.
package config

import (
	"os"
	"path/filepath"
)

const (
	ColabDomain  = "https://colab.research.google.com"
	ClientID     = "REDACTED-OAUTH-CLIENT-ID"
	ClientSecret = "REDACTED-OAUTH-CLIENT-SECRET"
	OAuthScope   = "profile email https://www.googleapis.com/auth/colaboratory"
	TokenURL     = "https://oauth2.googleapis.com/token"
	AuthURL      = "https://accounts.google.com/o/oauth2/v2/auth"

	TunBase           = "/tun/m"
	KeepaliveInterval = 300 // seconds

	HProxyToken = "X-Colab-Runtime-Proxy-Token"
	HXsrf       = "X-Goog-Colab-Token"
	HAgent      = "X-Colab-Client-Agent"
	HAgentVal   = "vscode"
	HTunnel     = "X-Colab-Tunnel"
	HTunnelVal  = "Google"
)

func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "colab-tui")
}

func AuthFile() string     { return filepath.Join(ConfigDir(), "auth.json") }
func SessionsFile() string { return filepath.Join(ConfigDir(), "sessions.json") }
func PidDir() string       { return filepath.Join(ConfigDir(), "pids") }
