package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// rcloneRemoteConfig returns the local `rclone config show <remote>` output (the
// "[remote]" section, including its token) so it can be pushed to a runtime.
// Generic over any locally-configured rclone remote name — no hardcoded remote.
func rcloneRemoteConfig(remote string) (string, error) {
	out, err := exec.Command("rclone", "config", "show", remote).Output()
	if err != nil {
		return "", fmt.Errorf("rclone config show %q failed — is the remote configured locally? (rclone listremotes): %w", remote, err)
	}
	cfg := strings.TrimSpace(string(out))
	if !strings.Contains(cfg, "["+remote+"]") {
		return "", fmt.Errorf("rclone remote %q not found locally (rclone listremotes)", remote)
	}
	return cfg, nil
}

// setupRcloneRemote installs rclone on the runtime (if missing) and writes the
// named remote into the runtime's ~/.config/rclone/rclone.conf — headless, no
// Google Drive consent browser. Idempotent and multi-remote-safe (re-pushing a
// remote replaces just its section). After this, the runtime can `rclone copy`
// to/from "<remote>:path" and scripts on the box can save their own outputs.
func setupRcloneRemote(baseURL, token, remote string) error {
	cfg, err := rcloneRemoteConfig(remote)
	if err != nil {
		return err
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(cfg + "\n"))
	script := fmt.Sprintf(`which rclone >/dev/null 2>&1 || (curl -s https://rclone.org/install.sh | bash >/dev/null 2>&1) || (cd /tmp && curl -sLO https://downloads.rclone.org/rclone-current-linux-amd64.zip && unzip -oq rclone-current-linux-amd64.zip && cp rclone-*-linux-amd64/rclone /usr/bin/ && chmod +x /usr/bin/rclone)
mkdir -p ~/.config/rclone
conf="$HOME/.config/rclone/rclone.conf"; touch "$conf"
echo %s | base64 -d > /tmp/_rc_new.conf
awk -v r="[%s]" 'BEGIN{skip=0} /^\[/{skip=($0==r)} !skip{print}' "$conf" > /tmp/_rc_keep.conf
cat /tmp/_rc_keep.conf /tmp/_rc_new.conf > "$conf"; rm -f /tmp/_rc_keep.conf /tmp/_rc_new.conf
echo "rclone $(rclone version 2>/dev/null | head -1)"
rclone lsd %s: >/dev/null 2>&1 && echo "OK: remote '%s' reachable from the runtime" || echo "WARN: remote '%s' not reachable (token/scope?)"`,
		b64, remote, remote, remote, remote)

	fmt.Printf("Setting up rclone remote %q on the runtime (headless, no consent)...\n", remote)
	return runtimeShell(baseURL, token, script, 300*time.Second, os.Stdout)
}

func newRcloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rclone",
		Short: "Headless rclone-remote access on a runtime (no Drive consent browser)",
		Long: `Push a local rclone remote (any configured remote name) onto a runtime so
it can copy files to/from "<remote>:path" with no browser consent and no
Drive-FUSE propagation lag. Use 'colab cp' for transfers, or let scripts on
the runtime call rclone directly to save their own outputs (e.g. training
checkpoints). 'colab rclone mount' is available when you want a browsable tree.`,
	}
	cmd.AddCommand(newRcloneSetupCmd(), newRcloneMountCmd())
	return cmd
}

func newRcloneSetupCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "setup REMOTE",
		Short: "Install rclone + push a local rclone remote onto the running runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			return setupRcloneRemote(s.BaseURL, s.Token, args[0])
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	return cmd
}

func newRcloneMountCmd() *cobra.Command {
	var (
		label      string
		mountpoint string
	)
	cmd := &cobra.Command{
		Use:   "mount REMOTE[:subpath]",
		Short: "FUSE-mount an rclone remote on the runtime (optional; copy is usually better)",
		Long: `Mounts "<remote>:subpath" at a runtime directory via 'rclone mount --daemon'.
Convenience for code with hardcoded output paths. For training/heavy IO prefer
'colab cp' (or have the script rclone-copy its own outputs) — FUSE is slower and
a runtime crash can lose writes still buffered in the VFS cache.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			src := args[0]
			if !strings.Contains(src, ":") {
				src += ":"
			}
			script := fmt.Sprintf(`mkdir -p %q
rclone mount %q %q --daemon --vfs-cache-mode writes --dir-cache-time 10s --allow-non-empty 2>&1
sleep 2; mountpoint -q %q && echo "mounted %s at %s" || echo "WARN: mount may have failed"`,
				mountpoint, src, mountpoint, mountpoint, src, mountpoint)
			return runtimeShell(s.BaseURL, s.Token, script, 60*time.Second, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "/content/rclone", "Runtime mount point")
	return cmd
}

// newCpCmd copies via rclone ON the runtime. Either side may be an rclone remote
// path ("<remote>:path") set up by 'colab rclone setup' / 'colab start --rclone',
// or a runtime-local path. No mount involved — a one-shot transfer.
func newCpCmd() *cobra.Command {
	var (
		label   string
		timeout int
	)
	cmd := &cobra.Command{
		Use:   "cp SRC DST",
		Short: "Copy files between an rclone remote and the runtime (rclone copy, no mount)",
		Long: `Runs 'rclone copy SRC DST' on the runtime. Examples:
  colab cp mydrive:data/x.mp4 /content/      # pull input to the box
  colab cp /content/runs/exp1 mydrive:runs/exp1         # push outputs back
Requires the remote to be set up first (colab rclone setup REMOTE, or
colab start --rclone REMOTE).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			echoTarget(s.Label)
			script := fmt.Sprintf(`rclone copy %q %q 2>&1 | tail -3; echo "[rclone copy exit $?]"`, args[0], args[1])
			return runtimeShell(s.BaseURL, s.Token, script, time.Duration(timeout)*time.Second, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().IntVar(&timeout, "timeout", 1800, "Timeout in seconds")
	return cmd
}
