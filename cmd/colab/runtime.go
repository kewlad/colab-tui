package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/netanelbi/colab-tui/internal/auth"
	"github.com/netanelbi/colab-tui/internal/config"
	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/netanelbi/colab-tui/internal/session"
	"github.com/spf13/cobra"
)

func loadAuthAndStore() (*auth.Manager, *session.Store, error) {
	m, err := auth.Load()
	if err != nil {
		return nil, nil, err
	}
	if m.RefreshToken == "" {
		return nil, nil, fmt.Errorf("not authenticated — run: colab auth login")
	}
	store := session.NewStore(config.SessionsFile())
	if err := store.Load(); err != nil {
		return nil, nil, fmt.Errorf("load sessions: %w", err)
	}
	return m, store, nil
}

func resolveSession(store *session.Store, label string) (*session.Session, error) {
	if label != "" {
		s := store.GetByLabel(label)
		if s == nil {
			return nil, fmt.Errorf("no session with label %q", label)
		}
		return s, nil
	}
	// No label: use the single runtime if there's exactly one, but REFUSE to
	// guess when several are assigned — silently picking the most-recent one
	// could run a command (or a delete) against the wrong machine.
	sessions := store.List()
	switch len(sessions) {
	case 0:
		return nil, fmt.Errorf("no active sessions — run: colab start")
	case 1:
		return sessions[0], nil
	default:
		labels := make([]string, 0, len(sessions))
		for _, s := range sessions {
			labels = append(labels, s.Label)
		}
		sort.Strings(labels)
		return nil, fmt.Errorf("%d runtimes assigned (%s) — specify which with --label <LABEL> (or a positional [LABEL])",
			len(sessions), strings.Join(labels, ", "))
	}
}

func newStartCmd() *cobra.Command {
	var (
		gpu          bool
		tpu          bool
		highmem      bool
		accelerator  string
		label        string
		gdrive       bool
		rcloneRemote string
	)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Assign a new Colab runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			client := api.NewClient(authMgr)
			client.Verbose = verbose

			variant := "DEFAULT"
			if gpu {
				variant = "GPU"
			} else if tpu {
				variant = "TPU"
			}
			if gpu && accelerator == "" {
				accelerator = "T4"
			}
			shape := ""
			if highmem {
				shape = "hm"
			}
			if label == "" {
				label = fmt.Sprintf("runtime-%d", time.Now().Unix())
			}

			fmt.Printf("Assigning %s runtime (label: %s)...\n", variant, label)
			info, err := client.Assign(context.Background(), variant, accelerator, shape)
			if err != nil {
				return fmt.Errorf("assign: %w", err)
			}

			sess := &session.Session{
				ID:          info.Endpoint,
				Label:       label,
				Endpoint:    info.Endpoint,
				Variant:     info.Variant,
				Accelerator: info.Accelerator,
				BaseURL:     info.BaseURL,
				Token:       info.Token,
				TokenExpiry: time.Now().Add(time.Duration(info.ExpiresIn) * time.Second),
				AssignedAt:  time.Now(),
			}

			fmt.Printf("Runtime assigned:\n")
			fmt.Printf("  Endpoint:    %s\n", info.Endpoint)
			fmt.Printf("  Variant:     %s\n", info.Variant)
			fmt.Printf("  Accelerator: %s\n", info.Accelerator)
			fmt.Printf("  Base URL:    %s\n", info.BaseURL)

			// Create the Jupyter session FIRST — before launching keepalive — so
			// a kernel already exists when the daemon connects. Otherwise the
			// daemon races ahead, finds zero kernels, and creates its OWN
			// session: a duplicate row in Colab's Active Sessions panel, and the
			// keepalive ends up holding that kernel while the labeled one is left
			// to idle-reap.
			fmt.Printf("Creating notebook session (%s.ipynb)...\n", label)
			kernelID, err := runtime.CreateSession(info.BaseURL, info.Token, label)
			if err != nil {
				fmt.Printf("  Warning: session creation failed: %v\n", err)
			} else {
				fmt.Printf("  Kernel:      %s\n", kernelID)
			}
			// Drop the reap-detection sentinel so `colab health` can tell a
			// recycled container (fresh VM, /content wiped) from a live one.
			writeRuntimeSentinel(info.BaseURL, info.Token, info.Endpoint, label)
			sess.SentinelSet = true

			// Persist the session before launching keepalive — the daemon reads
			// the store (by endpoint) to get the base URL + token.
			store.Add(sess)
			if err := store.Save(); err != nil {
				return fmt.Errorf("save session: %w", err)
			}

			// Spawn keepalive as a detached subprocess. It now finds the kernel
			// created above and HOLDS it (never creating a duplicate).
			kaCmd := exec.Command(os.Args[0], "keepalive", "--endpoint", info.Endpoint)
			kaCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			kaCmd.Env = append(os.Environ(), "COLAB_PROFILE="+config.Profile())
			kaCmd.Stdout = nil
			kaCmd.Stderr = nil
			if err := kaCmd.Start(); err != nil {
				fmt.Printf("Warning: failed to start keepalive: %v\n", err)
			} else {
				sess.KeepalivePID = kaCmd.Process.Pid
				go kaCmd.Wait() // detach — don't block
			}
			fmt.Printf("  Keepalive:   PID %d\n", sess.KeepalivePID)
			_ = store.Save()

			if gdrive {
				fmt.Println()
				if err := mountDrive(context.Background(), client, info.Endpoint, info.BaseURL, info.Token, label, "/content/drive", 120*time.Second); err != nil {
					fmt.Printf("Warning: Drive mount failed: %v\n", err)
				}
			}
			if rcloneRemote != "" {
				fmt.Println()
				if err := setupRcloneRemote(info.BaseURL, info.Token, label, rcloneRemote); err != nil {
					fmt.Printf("Warning: rclone setup failed: %v\n", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&gpu, "gpu", false, "Request GPU runtime")
	cmd.Flags().BoolVar(&tpu, "tpu", false, "Request TPU runtime")
	cmd.Flags().BoolVar(&highmem, "highmem", false, "Request high-memory shape")
	cmd.Flags().StringVar(&accelerator, "accelerator", "", "Accelerator type (e.g. T4, A100, L4)")
	cmd.Flags().StringVar(&label, "label", "", "Label for this runtime")
	cmd.Flags().BoolVar(&gdrive, "gdrive", false, "Mount Google Drive after starting")
	cmd.Flags().StringVar(&rcloneRemote, "rclone", "", "Headless: install rclone + push this local rclone remote (no consent browser)")
	return cmd
}

func newStopCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "stop [LABEL]",
		Short: "Stop and unassign a runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			client := api.NewClient(authMgr)

			var sessions []*session.Session
			if all {
				sessions = store.List()
			} else {
				label := ""
				if len(args) > 0 {
					label = args[0]
				}
				s, err := resolveSession(store, label)
				if err != nil {
					return err
				}
				sessions = []*session.Session{s}
			}

			for _, s := range sessions {
				fmt.Printf("Stopping %s (endpoint: %s)...\n", s.Label, s.Endpoint)
				// Kill keepalive
				if s.KeepalivePID > 0 {
					if proc, err := os.FindProcess(s.KeepalivePID); err == nil {
						proc.Signal(syscall.SIGTERM)
					}
				}
				// Unassign
				if err := client.Unassign(context.Background(), s.Endpoint); err != nil {
					fmt.Printf("  Warning: unassign failed: %v\n", err)
				}
				store.Remove(s.Label)
			}
			return store.Save()
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Stop all runtimes")
	return cmd
}

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active runtimes (reconciled with Colab's server-side assignments)",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			sessions := store.List()

			// Server-side truth: what Colab actually has assigned to us right now.
			// The local file only adds the label/keepalive we can't get back.
			client := api.NewClient(authMgr)
			client.Verbose = verbose
			assignments, aerr := client.ListAssignments(context.Background())
			localByEndpoint := map[string]*session.Session{}
			for _, s := range sessions {
				localByEndpoint[s.Endpoint] = s
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "LABEL\tVARIANT\tACCELERATOR\tSHAPE\tENDPOINT\tKEEPALIVE\tSERVER")

			seen := map[string]bool{}
			if aerr == nil {
				for _, a := range assignments {
					seen[a.Endpoint] = true
					label, ka := "-", "-"
					if s := localByEndpoint[a.Endpoint]; s != nil {
						label = s.Label
						ka = "dead"
						if session.IsKeepaliveRunning(s.Endpoint) {
							ka = fmt.Sprintf("alive (PID %d)", s.KeepalivePID)
						}
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
						label, a.Variant, a.Accelerator, a.MachineShape, a.Endpoint, ka, "assigned")
				}
			}
			// Reconcile: a local entry the server no longer lists is an orphan
			// (its runtime was reaped/unassigned). When the server query
			// succeeded, prune them — that's the authoritative signal. When it
			// failed, keep them and just mark status unknown.
			var pruned []string
			for _, s := range sessions {
				if seen[s.Endpoint] {
					continue
				}
				if aerr == nil {
					if s.KeepalivePID > 0 {
						if proc, err := os.FindProcess(s.KeepalivePID); err == nil {
							proc.Signal(syscall.SIGTERM)
						}
					}
					store.Remove(s.Label)
					pruned = append(pruned, s.Label)
					continue
				}
				ka := "dead"
				if session.IsKeepaliveRunning(s.Endpoint) {
					ka = fmt.Sprintf("alive (PID %d)", s.KeepalivePID)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Label, s.Variant, s.Accelerator, "-", s.Endpoint, ka, "?")
			}
			w.Flush()

			if len(pruned) > 0 {
				_ = store.Save()
				fmt.Fprintf(os.Stderr, "\n(pruned %d stale entries no longer assigned server-side: %s)\n",
					len(pruned), strings.Join(pruned, ", "))
			}
			if aerr != nil {
				fmt.Fprintf(os.Stderr, "\n(server assignment check failed: %v — showing local state only)\n", aerr)
			} else if len(assignments) == 0 {
				fmt.Println("No active runtimes.")
			}
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [LABEL]",
		Short: "Detailed runtime status",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			label := ""
			if len(args) > 0 {
				label = args[0]
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			fmt.Printf("Label:       %s\n", s.Label)
			fmt.Printf("Endpoint:    %s\n", s.Endpoint)
			fmt.Printf("Variant:     %s\n", s.Variant)
			fmt.Printf("Accelerator: %s\n", s.Accelerator)
			fmt.Printf("Base URL:    %s\n", s.BaseURL)
			fmt.Printf("Assigned:    %s (%s ago)\n", s.AssignedAt.Format(time.RFC3339), time.Since(s.AssignedAt).Round(time.Second))

			ttl := time.Until(s.TokenExpiry)
			if ttl > 0 {
				fmt.Printf("Token TTL:   %s\n", ttl.Round(time.Second))
			} else {
				fmt.Printf("Token TTL:   expired\n")
			}

			kaStatus := "dead"
			if session.IsKeepaliveRunning(s.Endpoint) {
				kaStatus = fmt.Sprintf("alive (PID %d)", s.KeepalivePID)
			}
			fmt.Printf("Keepalive:   %s\n", kaStatus)

			// Kernel state via the REST API (no WebSocket handshake) — instant
			// even when the kernel is busy. Opening a kernel WS here would block
			// on the kernel_info handshake until a running cell finishes.
			if _, state, err := runtime.FirstKernelState(s.BaseURL, s.Token); err == nil {
				fmt.Printf("Kernel:      %s\n", state)
			} else {
				fmt.Printf("Kernel:      none\n")
			}
			_ = authMgr // used for potential token refresh
			return nil
		},
	}
}

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume [LABEL]",
		Short: "Reconnect to an existing runtime (refresh proxy token)",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			label := ""
			if len(args) > 0 {
				label = args[0]
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			client := api.NewClient(authMgr)
			pt, err := client.RefreshProxyToken(context.Background(), s.Endpoint)
			if err != nil {
				return fmt.Errorf("refresh proxy token: %w", err)
			}

			s.Token = pt.Token
			s.BaseURL = pt.URL
			s.TokenExpiry = time.Now().Add(time.Duration(pt.ExpiresIn) * time.Second)
			fmt.Printf("Resumed %s — new token expires in %s\n", s.Label, time.Duration(pt.ExpiresIn)*time.Second)

			// Ensure a kernel exists BEFORE (re)starting keepalive, so the daemon
			// holds the labeled kernel rather than racing ahead and creating its
			// own. Reuse the live kernel if there is one — create only when the
			// runtime CONFIRMS it has none (fresh / reaped). A transient list
			// error is left alone: keepalive will reconcile on reconnect.
			if _, _, kerr := runtime.FirstKernelState(pt.URL, pt.Token); errors.Is(kerr, runtime.ErrNoKernels) {
				fmt.Printf("Creating notebook session (%s.ipynb)...\n", s.Label)
				if kernelID, cerr := runtime.CreateSession(pt.URL, pt.Token, s.Label); cerr != nil {
					fmt.Printf("  Warning: session creation failed: %v\n", cerr)
				} else {
					fmt.Printf("  Kernel: %s\n", kernelID)
				}
			} else {
				fmt.Printf("Kernel already running — reusing it.\n")
			}
			writeRuntimeSentinel(pt.URL, pt.Token, s.Endpoint, s.Label)
			s.SentinelSet = true

			// Restart keepalive if dead — it now finds the kernel above and holds it.
			if !session.IsKeepaliveRunning(s.Endpoint) {
				kaCmd := exec.Command(os.Args[0], "keepalive", "--endpoint", s.Endpoint)
				kaCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				kaCmd.Env = append(os.Environ(), "COLAB_PROFILE="+config.Profile())
				if err := kaCmd.Start(); err != nil {
					fmt.Printf("Warning: failed to restart keepalive: %v\n", err)
				} else {
					s.KeepalivePID = kaCmd.Process.Pid
					go kaCmd.Wait()
				}
			}

			if err := store.Save(); err != nil {
				return err
			}
			return nil
		},
	}
}

func newRestartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [LABEL]",
		Short: "Restart the Jupyter kernel (frees GPU memory)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			label := ""
			if len(args) > 0 {
				label = args[0]
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			fmt.Printf("Restarting kernel on %s...\n", s.Label)
			k, err := runtime.NewKernel(s.BaseURL, s.Token, s.Label)
			if err != nil {
				return fmt.Errorf("connect kernel: %w", err)
			}
			defer k.Close()

			if err := k.Restart(); err != nil {
				return fmt.Errorf("restart kernel: %w", err)
			}
			fmt.Println("Kernel restarted. GPU memory freed.")
			return nil
		},
	}
}

func newInterruptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "interrupt [LABEL]",
		Short: "Interrupt the running cell (KeyboardInterrupt), keeping the kernel + variables",
		Long: `Sends an interrupt to the running cell — like pressing Ctrl-C in a notebook.
The cell stops with KeyboardInterrupt but the kernel keeps running (imports and
variables survive), unlike 'colab restart' which tears the kernel down. (Ctrl-C in
'colab watch --stream'/'colab attach' only detaches the viewer; the cell keeps
running — use this to actually stop it.)`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			label := ""
			if len(args) > 0 {
				label = args[0]
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			echoTarget(s.Label)
			if err := runtime.InterruptKernel(s.BaseURL, s.Token); err != nil {
				return err
			}
			fmt.Println("Interrupt sent — the running cell should stop (kernel and variables preserved).")
			return nil
		},
	}
}

func newKeepaliveCmd() *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:    "keepalive",
		Short:  "Background keepalive daemon (internal)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := auth.Load()
			if err != nil {
				return err
			}
			return session.RunKeepalive(context.Background(), m, endpoint)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Runtime endpoint")
	cmd.MarkFlagRequired("endpoint")
	return cmd
}
