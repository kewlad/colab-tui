// Package auth implements OAuth2+PKCE authentication for Google APIs.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/netanelbi/colab-tui/internal/config"
)

// Manager holds OAuth2 credentials and manages token lifecycle.
type Manager struct {
	RefreshToken string `json:"refresh_token"`
	Email        string `json:"email,omitempty"` // account email, for display only
	accessToken  string
	tokenExpiry  time.Time
	mu           sync.Mutex
}

// Load reads an auth Manager from disk. If no path is provided, config.AuthFile() is used.
// Returns an empty Manager (not an error) if the file does not exist.
func Load(path ...string) (*Manager, error) {
	p := config.AuthFile()
	if len(path) > 0 {
		p = path[0]
	}

	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Manager{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read %s: %w", p, err)
	}

	var m Manager
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("auth: parse %s: %w", p, err)
	}
	return &m, nil
}

// Save writes the Manager's persistent fields to disk. If no path is provided,
// config.AuthFile() is used. The file is created with 0600 permissions.
func (m *Manager) Save(path ...string) error {
	p := config.AuthFile()
	if len(path) > 0 {
		p = path[0]
	}

	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return fmt.Errorf("auth: mkdir %s: %w", filepath.Dir(p), err)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("auth: marshal: %w", err)
	}

	if err := os.WriteFile(p, data, 0600); err != nil {
		return fmt.Errorf("auth: write %s: %w", p, err)
	}
	return nil
}

// Login performs the full OAuth2+PKCE flow, saving the refresh token on success.
func (m *Manager) Login(ctx context.Context) error {
	// Resolve credentials first: fail before opening a browser or a listener.
	creds, err := config.OAuth()
	if err != nil {
		return err
	}

	// Generate PKCE verifier (64 random bytes → base64url, no padding).
	raw := make([]byte, 64)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("auth: generate verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(raw)

	// SHA-256 challenge.
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Start local redirect server on a random port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("auth: listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error")
			http.Error(w, "Authentication failed: "+errMsg, http.StatusBadRequest)
			errCh <- fmt.Errorf("auth: oauth callback error: %s", errMsg)
			return
		}
		fmt.Fprintln(w, "Authentication successful! You may close this tab.")
		codeCh <- code
	})

	srv := &http.Server{Handler: mux}
	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("auth: callback server: %w", err)
		}
	}()
	defer srv.Close()

	// Build authorization URL.
	params := url.Values{
		"client_id":             {creds.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {config.OAuthScope},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	authURL := config.AuthURL + "?" + params.Encode()

	fmt.Println("Opening browser for Google OAuth...")
	if err := openBrowser(authURL); err != nil {
		fmt.Printf("Open this URL in your browser:\n%s\n", authURL)
	}

	// Wait for auth code with 120s timeout.
	timeout := time.NewTimer(120 * time.Second)
	defer timeout.Stop()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-timeout.C:
		return fmt.Errorf("auth: login timed out waiting for browser callback")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Exchange code + verifier for tokens.
	tokens, err := exchangeCode(ctx, code, verifier, redirectURI)
	if err != nil {
		return err
	}

	m.mu.Lock()
	m.RefreshToken = tokens.RefreshToken
	m.accessToken = tokens.AccessToken
	m.tokenExpiry = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	m.Email = fetchEmail(ctx, tokens.AccessToken) // best-effort, for display
	m.mu.Unlock()

	return m.Save()
}

// GetAccessToken returns a valid access token, refreshing it if necessary.
// It is safe for concurrent use.
func (m *Manager) GetAccessToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && time.Now().Before(m.tokenExpiry.Add(-30*time.Second)) {
		return m.accessToken, nil
	}

	if m.RefreshToken == "" {
		return "", fmt.Errorf("auth: not logged in — run 'colab-tui auth login'")
	}

	tokens, err := refreshAccessToken(ctx, m.RefreshToken)
	if err != nil {
		return "", err
	}

	m.accessToken = tokens.AccessToken
	m.tokenExpiry = time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second)
	return m.accessToken, nil
}

// tokenResponse is the JSON body returned by Google's token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

func exchangeCode(ctx context.Context, code, verifier, redirectURI string) (*tokenResponse, error) {
	creds, err := config.OAuth()
	if err != nil {
		return nil, err
	}
	body := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"code":          {code},
		"code_verifier": {verifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	return postToken(ctx, body)
}

func refreshAccessToken(ctx context.Context, refreshToken string) (*tokenResponse, error) {
	creds, err := config.OAuth()
	if err != nil {
		return nil, err
	}
	body := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	return postToken(ctx, body)
}

func postToken(ctx context.Context, vals url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.TokenURL,
		strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, fmt.Errorf("auth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth: token request: %w", err)
	}
	defer resp.Body.Close()

	var tok tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return nil, fmt.Errorf("auth: decode token response: %w", err)
	}
	if tok.Error != "" {
		return nil, fmt.Errorf("auth: token error %s: %s", tok.Error, tok.ErrorDesc)
	}
	return &tok, nil
}

// fetchEmail looks up the account email via Google's userinfo endpoint.
// Best-effort: returns "" on any error (the email is only used for display).
func fetchEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://www.googleapis.com/oauth2/v3/userinfo", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return ""
	}
	return info.Email
}

func openBrowser(u string) error {
	var cmd string
	switch runtime.GOOS {
	case "linux":
		cmd = "xdg-open"
	case "darwin":
		cmd = "open"
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
	return exec.Command(cmd, u).Start()
}
