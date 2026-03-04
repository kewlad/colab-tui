package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/netanelbi/colab-tui/internal/auth"
	"github.com/netanelbi/colab-tui/internal/config"
)

// tokenRefreshBuffer is how long before token expiry we should refresh.
const tokenRefreshBuffer = 5 * time.Minute

func pidPath(endpoint string) string {
	safe := strings.ReplaceAll(endpoint, "/", "_")
	return filepath.Join(config.PidDir(), safe+".pid")
}

// WritePID writes the current process PID to the endpoint's PID file.
func WritePID(endpoint string) error {
	path := pidPath(endpoint)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600)
}

// RemovePID deletes the PID file for the given endpoint.
func RemovePID(endpoint string) {
	_ = os.Remove(pidPath(endpoint))
}

// ReadPID reads the PID from the endpoint's PID file.
func ReadPID(endpoint string) (int, error) {
	data, err := os.ReadFile(pidPath(endpoint))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid PID in file: %w", err)
	}
	return pid, nil
}

// IsKeepaliveRunning returns true if a keepalive process for the endpoint is
// alive (PID file exists and the process responds to kill -0).
func IsKeepaliveRunning(endpoint string) bool {
	pid, err := ReadPID(endpoint)
	if err != nil {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// refreshProxyTokenIfNeeded checks the session's token expiry and refreshes
// the proxy token if it's within tokenRefreshBuffer of expiring. It updates
// the session store on disk so other commands pick up the new token.
func refreshProxyTokenIfNeeded(ctx context.Context, client *api.Client, store *Store, endpoint string) {
	sess := findByEndpoint(store, endpoint)
	if sess == nil {
		return
	}

	ttl := time.Until(sess.TokenExpiry)
	if ttl > tokenRefreshBuffer {
		return
	}

	log.Printf("keepalive: proxy token expires in %s, refreshing...", ttl.Round(time.Second))
	pt, err := client.RefreshProxyToken(ctx, endpoint)
	if err != nil {
		log.Printf("keepalive: proxy token refresh failed: %v", err)
		return
	}

	sess.Token = pt.Token
	sess.BaseURL = pt.URL
	sess.TokenExpiry = time.Now().Add(time.Duration(pt.ExpiresIn) * time.Second)

	if err := store.Save(); err != nil {
		log.Printf("keepalive: failed to save refreshed token: %v", err)
	} else {
		log.Printf("keepalive: proxy token refreshed, expires in %ds", pt.ExpiresIn)
	}
}

// findByEndpoint returns the session matching the given endpoint.
func findByEndpoint(store *Store, endpoint string) *Session {
	for _, s := range store.List() {
		if s.Endpoint == endpoint {
			return s
		}
	}
	return nil
}

// RunKeepalive is a long-lived loop that pings the Colab runtime every
// KeepaliveInterval seconds and refreshes the proxy token before it expires.
// It writes its PID on startup and removes it on exit.
func RunKeepalive(ctx context.Context, authMgr *auth.Manager, endpoint string) error {
	if err := WritePID(endpoint); err != nil {
		return fmt.Errorf("write PID: %w", err)
	}
	defer RemovePID(endpoint)

	client := api.NewClient(authMgr)

	// Load session store for proxy token refresh
	store := NewStore(config.SessionsFile())
	if err := store.Load(); err != nil {
		log.Printf("keepalive: warning: could not load session store: %v", err)
	}

	ticker := time.NewTicker(time.Duration(config.KeepaliveInterval) * time.Second)
	defer ticker.Stop()

	log.Printf("keepalive: started for endpoint %s (pid %d)", endpoint, os.Getpid())

	for {
		select {
		case <-ctx.Done():
			log.Printf("keepalive: stopping for endpoint %s", endpoint)
			return nil
		case <-ticker.C:
			if err := client.SendKeepalive(ctx, endpoint); err != nil {
				log.Printf("keepalive: error for endpoint %s: %v", endpoint, err)
			} else {
				log.Printf("keepalive: pinged endpoint %s", endpoint)
			}

			// Reload store to get latest state, then refresh token if needed
			if err := store.Load(); err == nil {
				refreshProxyTokenIfNeeded(ctx, client, store, endpoint)
			}
		}
	}
}
