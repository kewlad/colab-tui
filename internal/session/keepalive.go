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
	"github.com/netanelbi/colab-tui/internal/runtime"
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
// the session store on disk so other commands pick up the new token. Returns
// true if the token was refreshed (so the caller can reconnect held sockets).
func refreshProxyTokenIfNeeded(ctx context.Context, client *api.Client, store *Store, endpoint string) bool {
	sess := findByEndpoint(store, endpoint)
	if sess == nil {
		return false
	}

	ttl := time.Until(sess.TokenExpiry)
	if ttl > tokenRefreshBuffer {
		return false
	}

	log.Printf("keepalive: proxy token expires in %s, refreshing...", ttl.Round(time.Second))
	pt, err := client.RefreshProxyToken(ctx, endpoint)
	if err != nil {
		log.Printf("keepalive: proxy token refresh failed: %v", err)
		return false
	}

	sess.Token = pt.Token
	sess.BaseURL = pt.URL
	sess.TokenExpiry = time.Now().Add(time.Duration(pt.ExpiresIn) * time.Second)

	if err := store.Save(); err != nil {
		log.Printf("keepalive: failed to save refreshed token: %v", err)
	} else {
		log.Printf("keepalive: proxy token refreshed, expires in %ds", pt.ExpiresIn)
	}
	return true
}

// runtimeGone reports whether the endpoint is no longer assigned server-side.
// A query error returns false (can't confirm — don't remove anything).
func runtimeGone(ctx context.Context, client *api.Client, endpoint string) bool {
	assignments, err := client.ListAssignments(ctx)
	if err != nil {
		return false
	}
	for _, a := range assignments {
		if a.Endpoint == endpoint {
			return false
		}
	}
	return true
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

// RunKeepalive is a long-lived loop that keeps a Colab runtime alive the way the
// VS Code extension does: it HOLDS A KERNEL WEBSOCKET CONNECTION OPEN (so the
// runtime always sees an attached client — "kernel connections > 0"), pings that
// connection every HeartbeatInterval to register activity, sends the HTTP
// keep-alive every KeepaliveInterval, and refreshes the proxy token before it
// expires (reconnecting the held connection with the new token). It writes its
// PID on startup and removes it on exit.
func RunKeepalive(ctx context.Context, authMgr *auth.Manager, endpoint string) error {
	if err := WritePID(endpoint); err != nil {
		return fmt.Errorf("write PID: %w", err)
	}
	defer RemovePID(endpoint)

	client := api.NewClient(authMgr)

	store := NewStore(config.SessionsFile())
	if err := store.Load(); err != nil {
		log.Printf("keepalive: warning: could not load session store: %v", err)
	}

	// held is the persistent kernel connection; nil means disconnected (a
	// reconnect is attempted on the next heartbeat tick).
	var held *runtime.KeepaliveConn
	defer func() {
		if held != nil {
			held.Close()
		}
	}()

	// connect (re)opens the held connection using the session's current token.
	connect := func() {
		if held != nil {
			held.Close()
			held = nil
		}
		sess := findByEndpoint(store, endpoint)
		if sess == nil {
			return
		}
		kc, err := runtime.DialKeepalive(sess.BaseURL, sess.Token, sess.Label)
		if err != nil {
			log.Printf("keepalive: hold-connection dial failed: %v", err)
			return
		}
		held = kc
		log.Printf("keepalive: holding kernel connection for %s", endpoint)
	}

	// deadCh returns the held connection's Dead channel, or nil (blocks forever
	// in select) when disconnected.
	deadCh := func() <-chan struct{} {
		if held == nil {
			return nil
		}
		return held.Dead()
	}

	connect()

	heartbeat := time.NewTicker(time.Duration(config.HeartbeatInterval) * time.Second)
	defer heartbeat.Stop()
	ping := time.NewTicker(time.Duration(config.KeepaliveInterval) * time.Second)
	defer ping.Stop()

	log.Printf("keepalive: started for endpoint %s (pid %d)", endpoint, os.Getpid())

	// cleanup tears down the held connection and removes the orphaned session.
	cleanup := func() {
		if held != nil {
			held.Close()
			held = nil
		}
		if err := store.Load(); err == nil {
			if s := findByEndpoint(store, endpoint); s != nil {
				store.Remove(s.Label)
				_ = store.Save()
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("keepalive: stopping for endpoint %s", endpoint)
			return nil

		case <-deadCh():
			log.Printf("keepalive: held connection dropped, reconnecting")
			held = nil
			connect()

		case <-heartbeat.C:
			// Keep the held connection warm; reconnect if it dropped. Death is
			// detected authoritatively on the ping cycle, not here.
			if held == nil {
				connect()
			} else if err := held.Ping(); err != nil {
				log.Printf("keepalive: heartbeat ping failed: %v", err)
				held.Close()
				held = nil
			}

		case <-ping.C:
			// Like the VS Code extension, RE-VERIFY liveness every cycle and stop
			// keeping a dead runtime alive: if Colab no longer lists this endpoint
			// as assigned, clean up and exit instead of pinging into the void.
			// (runtimeGone returns false on a query error, so a transient network
			// blip never wrongly kills a live session.)
			if runtimeGone(ctx, client, endpoint) {
				log.Printf("keepalive: endpoint %s no longer assigned; removing session and exiting", endpoint)
				cleanup()
				return nil
			}
			// Send the tunnel keep-alive UNCONDITIONALLY — it is the cheap
			// mechanism that actually refreshes the idle timer (TFE stamps
			// LastActiveTime on arrival) and it needs no kernel. Gating it on the
			// held connection, as the VS Code extension does, is right for an IDE
			// (let runtimes nobody is attached to die) and wrong here: you asked
			// for this runtime, so a proxy blip or a dead kernel that blocks the
			// re-dial must not also stop the pings and let it be idle-pruned.
			// Log the success too, not just the failure: the daemon's output is
			// discarded in normal use, so this line is what makes "was it
			// actually pinging?" answerable when you run it in the foreground to
			// diagnose an unexpected reap.
			if err := client.SendKeepalive(ctx, endpoint); err != nil {
				log.Printf("keepalive: HTTP keep-alive error for %s: %v", endpoint, err)
			} else {
				log.Printf("keepalive: ping OK for %s (held=%t)", endpoint, held != nil)
			}
			if held == nil {
				connect()
			}
			// Reload store, refresh the token if needed, and reconnect the held
			// connection with the fresh token if it changed.
			if err := store.Load(); err == nil {
				if refreshProxyTokenIfNeeded(ctx, client, store, endpoint) {
					connect()
				}
			}
		}
	}
}
