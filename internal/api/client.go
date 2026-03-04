// Package api provides a client for the Colab runtime management API.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/netanelbi/colab-tui/internal/auth"
	"github.com/netanelbi/colab-tui/internal/config"
)

// MakeNBH converts a UUID string into the notebook handle format used by the
// Colab API: dashes replaced with underscores, padded with dots to length 44.
func MakeNBH(u string) string {
	s := strings.ReplaceAll(u, "-", "_")
	for len(s) < 44 {
		s += "."
	}
	return s
}

// StripXSS removes the ")]}'\n" XSSI prefix that Google APIs prepend to JSON
// responses as an XSS defense.
func StripXSS(body string) string {
	const prefix = ")]}'\n"
	if strings.HasPrefix(body, prefix) {
		return body[len(prefix):]
	}
	return body
}

// RuntimeInfo holds the result of a successful runtime assignment.
type RuntimeInfo struct {
	Endpoint    string
	BaseURL     string
	Token       string
	ExpiresIn   int
	Variant     string
	Accelerator string
}

// Assignment represents a currently allocated Colab runtime.
type Assignment struct {
	Endpoint     string
	Variant      string
	Accelerator  string
	MachineShape string
}

// ProxyToken holds a short-lived proxy token for WebSocket connections.
type ProxyToken struct {
	Token     string
	URL       string
	ExpiresIn int
}

// Client is the Colab API client.
type Client struct {
	Auth    *auth.Manager
	HTTP    *http.Client
	Verbose bool
}

// NewClient creates a new API client using the given auth manager.
func NewClient(authMgr *auth.Manager) *Client {
	return &Client{
		Auth: authMgr,
		HTTP: &http.Client{},
	}
}

// doRequest performs an HTTP request against the Colab API, injecting auth
// and standard headers, and stripping the XSS prefix from the response body.
func (c *Client) doRequest(ctx context.Context, method, path string, headers map[string]string) ([]byte, error) {
	fullURL := config.ColabDomain + path
	// Google requires authuser param on all colab domain requests
	if strings.Contains(fullURL, "?") {
		fullURL += "&authuser=0"
	} else {
		fullURL += "?authuser=0"
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	token, err := c.Auth.GetAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("get auth token: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(config.HAgent, config.HAgentVal)

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] %s %s\n", method, fullURL)
		for k, v := range req.Header {
			if k == "Authorization" {
				fmt.Fprintf(os.Stderr, "[DEBUG]   %s: Bearer %s...%s\n", k, v[0][7:15], v[0][len(v[0])-4:])
			} else {
				fmt.Fprintf(os.Stderr, "[DEBUG]   %s: %s\n", k, v[0])
			}
		}
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if c.Verbose {
		fmt.Fprintf(os.Stderr, "[DEBUG] Response: %d (%d bytes)\n", resp.StatusCode, len(body))
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return []byte(StripXSS(string(body))), nil
}

// Assign requests a new Colab runtime. It handles the XSRF two-step and
// interprets the outcome field before fetching a proxy token.
func (c *Client) Assign(ctx context.Context, variant, accelerator, shape string) (*RuntimeInfo, error) {
	u := uuid.New().String()
	nbh := MakeNBH(u)

	if variant == "" {
		variant = "DEFAULT"
	}
	path := config.TunBase + "/assign?nbh=" + nbh
	if variant != "DEFAULT" {
		path += "&variant=" + variant
	}
	if accelerator != "" {
		path += "&accelerator=" + accelerator
	}
	if shape != "" {
		path += "&shape=" + shape
	}

	body, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("assign GET: %w", err)
	}

	// Use raw JSON since variant can be string or number
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse assign response: %w", err)
	}

	// Extract XSRF token from GET response
	var xsrfToken string
	if t, ok := raw["token"]; ok {
		json.Unmarshal(t, &xsrfToken)
	}

	// If XSRF token present, repeat as POST
	if xsrfToken != "" {
		body, err = c.doRequest(ctx, http.MethodPost, path, map[string]string{
			config.HXsrf: xsrfToken,
		})
		if err != nil {
			return nil, fmt.Errorf("assign POST: %w", err)
		}
		raw = nil
		if err := json.Unmarshal(body, &raw); err != nil {
			return nil, fmt.Errorf("parse assign POST response: %w", err)
		}
	}

	// Check outcome: 0, 4, or nil = ok; 1/2 = quota error; 5 = banned
	if o, ok := raw["outcome"]; ok {
		var outcome int
		if json.Unmarshal(o, &outcome) == nil {
			switch outcome {
			case 1, 2:
				return nil, fmt.Errorf("quota exceeded (outcome=%d)", outcome)
			case 5:
				return nil, fmt.Errorf("account banned (outcome=5)")
			}
		}
	}

	var endpoint string
	if e, ok := raw["endpoint"]; ok {
		json.Unmarshal(e, &endpoint)
	}
	if endpoint == "" {
		return nil, fmt.Errorf("assign returned empty endpoint, response: %s", string(body))
	}

	pt, err := c.RefreshProxyToken(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("get proxy token: %w", err)
	}

	// Variant may be string ("GPU") or number (0=DEFAULT, 1=GPU, 2=TPU)
	variantStr := variant
	if v, ok := raw["variant"]; ok {
		var vs string
		if json.Unmarshal(v, &vs) == nil {
			variantStr = vs
		} else {
			var vi int
			if json.Unmarshal(v, &vi) == nil {
				switch vi {
				case 0:
					variantStr = "DEFAULT"
				case 1:
					variantStr = "GPU"
				case 2:
					variantStr = "TPU"
				}
			}
		}
	}

	return &RuntimeInfo{
		Endpoint:    endpoint,
		BaseURL:     pt.URL,
		Token:       pt.Token,
		ExpiresIn:   pt.ExpiresIn,
		Variant:     variantStr,
		Accelerator: accelerator,
	}, nil
}

// ListAssignments returns all currently active runtime assignments.
func (c *Client) ListAssignments(ctx context.Context) ([]Assignment, error) {
	body, err := c.doRequest(ctx, http.MethodGet, config.TunBase+"/assignments", nil)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}

	var resp struct {
		Assignments []struct {
			Endpoint     string `json:"endpoint"`
			Variant      string `json:"variant"`
			Accelerator  string `json:"accelerator"`
			MachineShape string `json:"machine_shape"`
		} `json:"assignments"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse assignments: %w", err)
	}

	out := make([]Assignment, 0, len(resp.Assignments))
	for _, a := range resp.Assignments {
		out = append(out, Assignment{
			Endpoint:     a.Endpoint,
			Variant:      a.Variant,
			Accelerator:  a.Accelerator,
			MachineShape: a.MachineShape,
		})
	}
	return out, nil
}

// RefreshProxyToken fetches a fresh proxy token for the given endpoint.
// The response may use either `tokenExpiresInSeconds` (int, from assign)
// or `tokenTtl` (string like "3600s", from refresh endpoint).
func (c *Client) RefreshProxyToken(ctx context.Context, endpoint string) (*ProxyToken, error) {
	path := config.TunBase + "/runtime-proxy-token?endpoint=" + endpoint + "&port=8080"

	body, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("refresh proxy token: %w", err)
	}

	var resp struct {
		Token              string `json:"token"`
		URL                string `json:"url"`
		TokenExpiresInSec  int    `json:"tokenExpiresInSeconds"`
		TokenTTL           string `json:"tokenTtl"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse proxy token: %w", err)
	}

	expiresIn := resp.TokenExpiresInSec
	if expiresIn <= 0 && resp.TokenTTL != "" {
		// Parse "3600s" format from refresh endpoint
		ttl := strings.TrimSuffix(resp.TokenTTL, "s")
		if n, err := strconv.Atoi(ttl); err == nil && n > 0 {
			expiresIn = n
		}
	}
	if expiresIn <= 0 {
		expiresIn = 3600 // default fallback
	}

	return &ProxyToken{
		Token:     resp.Token,
		URL:       resp.URL,
		ExpiresIn: expiresIn,
	}, nil
}

// Unassign releases a runtime. It performs the XSRF GET/POST dance.
func (c *Client) Unassign(ctx context.Context, endpoint string) error {
	path := config.TunBase + "/unassign/" + endpoint

	body, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return fmt.Errorf("unassign GET: %w", err)
	}

	var xsrf struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &xsrf); err != nil {
		return fmt.Errorf("parse unassign XSRF: %w", err)
	}

	_, err = c.doRequest(ctx, http.MethodPost, path, map[string]string{
		config.HXsrf: xsrf.Token,
	})
	if err != nil {
		return fmt.Errorf("unassign POST: %w", err)
	}
	return nil
}

// CredentialsPropagationResult holds the result of a credentials propagation request.
type CredentialsPropagationResult struct {
	Success              bool   `json:"success"`
	UnauthorizedRedirect string `json:"unauthorized_redirect_uri"`
}

// PropagateCredentials propagates ephemeral credentials (e.g. Drive) to the
// runtime's metadata server. Uses the same XSRF GET/POST pattern as other
// Colab endpoints.
//
// When dryRun is true, it checks whether credentials are already propagated.
// If success is false and UnauthorizedRedirect is non-empty, the user must
// complete a one-time browser consent at that URL before retrying with
// dryRun=false.
func (c *Client) PropagateCredentials(ctx context.Context, endpoint, authType string, dryRun bool) (*CredentialsPropagationResult, error) {
	path := fmt.Sprintf("%s/credentials-propagation/%s?authtype=%s&version=2&dryrun=%t&propagate=true&record=false",
		config.TunBase, endpoint, authType, dryRun)

	// GET to obtain XSRF token
	body, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("credentials propagation GET: %w", err)
	}

	var xsrf struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &xsrf); err != nil {
		return nil, fmt.Errorf("parse credentials XSRF: %w", err)
	}

	if xsrf.Token == "" {
		return nil, fmt.Errorf("no XSRF token in credentials propagation response")
	}

	// POST with XSRF token
	body, err = c.doRequest(ctx, http.MethodPost, path, map[string]string{
		config.HXsrf: xsrf.Token,
	})
	if err != nil {
		return nil, fmt.Errorf("credentials propagation POST: %w", err)
	}

	var result CredentialsPropagationResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse credentials propagation result: %w", err)
	}

	return &result, nil
}

// SendKeepalive pings the runtime to prevent it from being reclaimed.
func (c *Client) SendKeepalive(ctx context.Context, endpoint string) error {
	path := config.TunBase + "/" + endpoint + "/keep-alive/"
	_, err := c.doRequest(ctx, http.MethodGet, path, map[string]string{
		config.HTunnel: config.HTunnelVal,
	})
	if err != nil {
		return fmt.Errorf("keepalive: %w", err)
	}
	return nil
}
