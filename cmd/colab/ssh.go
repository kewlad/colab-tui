package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/spf13/cobra"
)

// sshHost is a placeholder hostname: the ProxyCommand supplies the transport,
// so ssh never resolves or connects to it. Only the login user matters.
const sshHost = "root@colab-runtime"

// remoteDir is where an interactive shell should land — Colab's working
// directory, not root's home, so `ls` shows what `fs put`/`cp` put there.
const remoteDir = "/content"

// sshKeyCandidates are scanned in order when --identity is not given. ssh-rsa
// keys are deliberately absent: the server rejects them, so auto-picking
// ~/.ssh/id_rsa would turn a working setup into an unexplained HTTP 400.
var sshKeyCandidates = []string{"id_ed25519.pub", "id_ecdsa.pub"}

func newSSHCmd() *cobra.Command {
	var (
		label     string
		proxyMode bool
		identity  string
	)
	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Open an SSH shell on the runtime (or bridge one for ~/.ssh/config)",
		Long: `Opens a real SSH session to the runtime over a WebSocket, so IDE
remote-development (VS Code Remote-SSH, JetBrains Gateway) and plain scp/rsync
work against Colab.

  colab ssh --label myrun              # interactive shell, starting in /content
  colab ssh --proxy-mode --label myrun # stdio bridge, for use as a ProxyCommand

For an ~/.ssh/config host (use an ABSOLUTE colab path — ssh runs the
ProxyCommand in a non-login shell where PATH may not include it):

  Host colab
    ProxyCommand /home/you/.local/bin/colab ssh --proxy-mode --label myrun
    User root
    StrictHostKeyChecking no
    UserKnownHostsFile /dev/null

Only ssh-ed25519 / ecdsa-sha2-nistp* keys are accepted; ssh-rsa is rejected
server-side. SSH is baked into the runtime image at creation, so a runtime that
answers HTTP 404 here cannot gain it without being recreated.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			pubkey, err := resolvePubkey(identity)
			if err != nil {
				return err
			}

			if proxyMode {
				// stdout is the raw SSH byte stream from here on — nothing else
				// may be written to it or the transport is corrupted.
				conn, err := runtime.DialSSH(s.BaseURL, s.Token, pubkey)
				if err != nil {
					return err
				}
				defer conn.Close()
				return runtime.BridgeStdio(conn)
			}

			echoTarget(s.Label)
			return interactiveSSH(s.Label, identity)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().BoolVar(&proxyMode, "proxy-mode", false, "Bridge the WebSocket to stdin/stdout as an OpenSSH ProxyCommand")
	cmd.Flags().StringVarP(&identity, "identity", "i", "", "Private key whose public key authenticates (default: ~/.ssh/id_ed25519, then id_ecdsa)")
	return cmd
}

// resolvePubkey returns the public key text to send verbatim in the pubkey
// header: derived from --identity when given, otherwise the first supported key
// found in ~/.ssh.
func resolvePubkey(identity string) (string, error) {
	if identity != "" {
		out, err := exec.Command("ssh-keygen", "-y", "-f", expandHome(identity)).Output()
		if err != nil {
			return "", fmt.Errorf("read public key from %s (is it an unencrypted private key?): %w", identity, err)
		}
		key := strings.TrimSpace(string(out))
		if key == "" {
			return "", fmt.Errorf("ssh-keygen produced no public key for %s", identity)
		}
		return key, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	for _, name := range sshKeyCandidates {
		data, err := os.ReadFile(filepath.Join(home, ".ssh", name))
		if err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", fmt.Errorf("no supported SSH key in ~/.ssh (looked for %s).\n"+
		"Create one with `ssh-keygen -t ed25519`, or pass --identity. "+
		"ssh-rsa keys are not scanned because the server rejects them",
		strings.Join(sshKeyCandidates, ", "))
}

// interactiveSSH spawns the system ssh client with this binary re-invoked as
// its ProxyCommand, so the interactive shell and --proxy-mode share one
// transport. It returns an exitCode carrying ssh's own status.
func interactiveSSH(label, identity string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the colab binary for ProxyCommand: %w", err)
	}

	// The bridge is a separate process, so it re-resolves the profile from
	// scratch — an active profile set only by --profile would be lost and the
	// bridge would look the runtime up in the wrong account's store.
	proxy := fmt.Sprintf("%q --profile %q ssh --proxy-mode --label %q", self, profile, label)
	if identity != "" {
		proxy += fmt.Sprintf(" --identity %q", identity)
	}

	args := []string{
		"-o", "ProxyCommand=" + proxy,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
	if identity != "" {
		args = append(args, "-i", expandHome(identity))
	}
	// -t forces a PTY, which a remote command otherwise suppresses; the cd is
	// tolerant so a runtime without /content still gets a shell.
	args = append(args, "-t", sshHost,
		fmt.Sprintf("cd %s 2>/dev/null; exec ${SHELL:-/bin/bash} -l", remoteDir))

	c := exec.Command("ssh", args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitCode(ee.ExitCode())
		}
		return fmt.Errorf("run ssh: %w", err)
	}
	return nil
}

// expandHome resolves a leading ~ so --identity accepts the path users type.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}
