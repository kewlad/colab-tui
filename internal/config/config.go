// Package config holds protocol constants and path helpers for the Colab CLI.
package config

import (
	"os"
	"path/filepath"
)

const (
	// ColabDomain serves the tunnel (/tun/m/*) and XSRF calls; ColabGapiDomain
	// serves the newer v1/* API (assignments, proxy-token, user-info). The VS
	// Code extension (v0.8.1) splits traffic across these two hosts.
	ColabDomain     = "https://colab.research.google.com"
	ColabGapiDomain = "https://colab.pa.googleapis.com"

	// ClientID/ClientSecret are the Colab OAuth "installed app" credentials
	// extracted from the extension (a public client secret, not a real secret).
	ClientID     = "REDACTED-OAUTH-CLIENT-ID"
	ClientSecret = "REDACTED-OAUTH-CLIENT-SECRET"
	OAuthScope   = "profile email https://www.googleapis.com/auth/colaboratory"
	TokenURL     = "https://oauth2.googleapis.com/token"
	AuthURL      = "https://accounts.google.com/o/oauth2/v2/auth"

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

func ConfigDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "colab-tui")
}

func AuthFile() string     { return filepath.Join(ConfigDir(), "auth.json") }
func SessionsFile() string { return filepath.Join(ConfigDir(), "sessions.json") }
func PidDir() string       { return filepath.Join(ConfigDir(), "pids") }
