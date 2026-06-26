package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/spf13/cobra"
)

// newWatchCmd is a cheap, reap-safe heartbeat monitor for a long-running job. It
// polls the kernel state + GPU via the REST/resources endpoints (never disturbs a
// busy kernel, survives reconnects) and exits when the running cell finishes or
// the runtime is gone. For a live byte-stream of the cell's stdout use `colab attach`.
func newWatchCmd() *cobra.Command {
	var (
		label    string
		interval int
		forever  bool
		logPath  string
	)
	cmd := &cobra.Command{
		Use:   "watch [LABEL]",
		Short: "Poll a runtime's kernel/GPU state until a running job finishes (reap-safe)",
		Long: `Heartbeat monitor for a long job. Every --interval seconds it prints the kernel
state and GPU utilization (via REST endpoints, so it never disturbs a busy kernel
and survives reaps) and exits when the kernel goes idle (cell finished) or the
runtime is gone. With --log it also tails the last line of a runtime log each tick.
For a live byte-stream of the running cell's stdout, use 'colab attach' instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			if len(args) > 0 {
				label = args[0]
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			client := api.NewClient(authMgr)

			first := true
			prevActive := false
			for {
				ts := time.Now().Format("15:04:05")
				_, state, kerr := runtime.FirstKernelState(s.BaseURL, s.Token)
				if kerr != nil {
					fmt.Printf("%s  kernel=GONE — runtime reaped or stopped\n", ts)
					return fmt.Errorf("runtime no longer reachable")
				}

				line := fmt.Sprintf("%s  kernel=%s", ts, state)
				if r, rerr := client.GetResources(context.Background(), s.BaseURL, s.Token); rerr == nil && len(r.GPUs) > 0 {
					g := r.GPUs[0]
					const gi = 1 << 30
					line += fmt.Sprintf("  gpu=%.0f%% %.1f/%.1fGiB", g.GPUUtilization*100,
						g.MemoryUsedBytes/gi, g.MemoryTotalBytes/gi)
				}
				if logPath != "" {
					if data, derr := runtime.GetFile(s.BaseURL, s.Token, logPath); derr == nil {
						if ll := lastLine(string(data)); ll != "" {
							line += "  | " + ll
						}
					}
				}
				fmt.Println(line)

				active := state != "idle" // busy / starting / restarting
				if !active && !forever {
					if prevActive {
						fmt.Printf("%s  kernel went idle — cell finished.\n", time.Now().Format("15:04:05"))
					} else if first {
						fmt.Println("(kernel idle — no cell running; pass --forever to keep watching)")
					}
					return nil
				}
				first, prevActive = false, active
				time.Sleep(time.Duration(interval) * time.Second)
			}
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().IntVar(&interval, "interval", 30, "Seconds between checks")
	cmd.Flags().BoolVar(&forever, "forever", false, "Keep watching even after the kernel goes idle")
	cmd.Flags().StringVar(&logPath, "log", "", "Also tail the last line of this runtime log file each tick")
	return cmd
}

// lastLine returns the final non-empty line of s.
func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}
