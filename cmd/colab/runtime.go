package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	s := store.GetDefault()
	if s == nil {
		return nil, fmt.Errorf("no active sessions — run: colab start")
	}
	return s, nil
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

			// Spawn keepalive as detached subprocess
			kaCmd := exec.Command(os.Args[0], "keepalive", "--endpoint", info.Endpoint)
			kaCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			kaCmd.Stdout = nil
			kaCmd.Stderr = nil
			if err := kaCmd.Start(); err != nil {
				fmt.Printf("Warning: failed to start keepalive: %v\n", err)
			} else {
				sess.KeepalivePID = kaCmd.Process.Pid
				// Detach — don't wait for keepalive
				go kaCmd.Wait()
			}

			store.Add(sess)
			if err := store.Save(); err != nil {
				return fmt.Errorf("save session: %w", err)
			}

			fmt.Printf("Runtime assigned:\n")
			fmt.Printf("  Endpoint:    %s\n", info.Endpoint)
			fmt.Printf("  Variant:     %s\n", info.Variant)
			fmt.Printf("  Accelerator: %s\n", info.Accelerator)
			fmt.Printf("  Base URL:    %s\n", info.BaseURL)
			fmt.Printf("  Keepalive:   PID %d\n", sess.KeepalivePID)

			// Create Jupyter session (notebook naming + kernel start)
			fmt.Printf("Creating notebook session (%s.ipynb)...\n", label)
			kernelID, err := runtime.CreateSession(info.BaseURL, info.Token, label)
			if err != nil {
				fmt.Printf("  Warning: session creation failed: %v\n", err)
			} else {
				fmt.Printf("  Kernel:      %s\n", kernelID)
			}
			// Drop the reap-detection sentinel so `colab health` can tell a
			// recycled container (fresh VM, /content wiped) from a live one.
			writeRuntimeSentinel(info.BaseURL, info.Token, info.Endpoint)

			if gdrive {
				fmt.Println()
				if err := mountDrive(context.Background(), client, info.Endpoint, info.BaseURL, info.Token, "/content/drive", 120*time.Second); err != nil {
					fmt.Printf("Warning: Drive mount failed: %v\n", err)
				}
			}
			if rcloneRemote != "" {
				fmt.Println()
				if err := setupRcloneRemote(info.BaseURL, info.Token, rcloneRemote); err != nil {
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
			// Local entries the server no longer lists = stale (reaped/unassigned).
			for _, s := range sessions {
				if seen[s.Endpoint] {
					continue
				}
				srv := "STALE (not assigned)"
				if aerr != nil {
					srv = "?" // couldn't reach the server
				}
				ka := "dead"
				if session.IsKeepaliveRunning(s.Endpoint) {
					ka = fmt.Sprintf("alive (PID %d)", s.KeepalivePID)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					s.Label, s.Variant, s.Accelerator, "-", s.Endpoint, ka, srv)
			}
			w.Flush()

			if aerr != nil {
				fmt.Fprintf(os.Stderr, "\n(server assignment check failed: %v — showing local state only)\n", aerr)
			} else if len(assignments) == 0 && len(sessions) == 0 {
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

			// Try to get kernel status
			k, err := runtime.NewKernel(s.BaseURL, s.Token)
			if err == nil {
				state, err := k.GetStatus()
				if err == nil {
					fmt.Printf("Kernel:      %s\n", state)
				}
				k.Close()
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

			// Restart keepalive if dead
			if !session.IsKeepaliveRunning(s.Endpoint) {
				kaCmd := exec.Command(os.Args[0], "keepalive", "--endpoint", s.Endpoint)
				kaCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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
			fmt.Printf("Resumed %s — new token expires in %s\n", s.Label, time.Duration(pt.ExpiresIn)*time.Second)

			// Re-create session if no kernel exists
			fmt.Printf("Ensuring notebook session (%s.ipynb)...\n", s.Label)
			kernelID, err := runtime.CreateSession(pt.URL, pt.Token, s.Label)
			if err != nil {
				fmt.Printf("  Warning: session creation failed: %v\n", err)
			} else {
				fmt.Printf("  Kernel: %s\n", kernelID)
			}
			writeRuntimeSentinel(pt.URL, pt.Token, s.Endpoint)
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
			k, err := runtime.NewKernel(s.BaseURL, s.Token)
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
