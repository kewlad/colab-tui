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

// RunKeepalive is a long-lived loop that pings the Colab runtime every
// KeepaliveInterval seconds. It writes its PID on startup and removes it on
// exit. Errors are logged but do not terminate the loop.
func RunKeepalive(ctx context.Context, authMgr *auth.Manager, endpoint string) error {
	if err := WritePID(endpoint); err != nil {
		return fmt.Errorf("write PID: %w", err)
	}
	defer RemovePID(endpoint)

	client := api.NewClient(authMgr)
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
		}
	}
}
