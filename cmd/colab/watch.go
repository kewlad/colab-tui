package main

import (
	"context"
	"fmt"
	"os"
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
		stream   bool
	)
	cmd := &cobra.Command{
		Use:   "watch [LABEL]",
		Short: "Monitor a running job — live output (--stream) or a status/GPU heartbeat",
		Long: `Watch a RUNNING job, two ways:

  colab watch --label myrun --stream                 # LIVE output of the running cell
                                                      #   (taps the kernel's stdout — no log file needed)
  colab watch --label myrun                          # status + GPU heartbeat each tick
  colab watch --label myrun --log /content/train.log # heartbeat + tail that runtime file
  colab watch --label myrun --forever                # keep polling even when idle

Use --stream when you don't have (or don't know) a log file — it shows whatever the
cell prints to stdout, exactly like 'colab attach'. The heartbeat modes use REST
endpoints, so they never disturb a busy kernel and survive reaps. All exit when the
cell finishes (or the runtime is gone). On an idle runtime there's nothing to watch.`,
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

			// --stream: show the running cell's live stdout (no log path needed),
			// guarded so it doesn't hang on an idle kernel (which emits nothing).
			if stream {
				_, state, kerr := runtime.FirstKernelState(s.BaseURL, s.Token)
				if kerr != nil {
					return fmt.Errorf("no kernel reachable — runtime reaped or stopped")
				}
				if state != "busy" {
					fmt.Println("kernel is idle — nothing running to stream. Start a job, then re-run with --stream.")
					return nil
				}
				k, err := runtime.NewKernelAttach(s.BaseURL, s.Token)
				if err != nil {
					return fmt.Errorf("connect kernel: %w", err)
				}
				defer k.Close()
				fmt.Fprintf(os.Stderr, "[%s] live output (joins from now; Ctrl-C to detach, the job keeps running)\n", s.Label)
				err = k.Attach(os.Stdout)
				fmt.Fprintln(os.Stderr, "— cell finished —")
				return err
			}

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
					} else {
						// Be loud, not silent — --log takes a runtime FILE PATH.
						line += fmt.Sprintf("  | (log %q not found on runtime)", logPath)
					}
				}
				fmt.Println(line)

				active := state != "idle" // busy / starting / restarting
				if !active && !forever {
					if prevActive {
						fmt.Printf("%s  kernel went idle — cell finished.\n", time.Now().Format("15:04:05"))
					} else if first {
						fmt.Println("kernel is idle — nothing running to watch.")
						fmt.Println("  • live output of a running cell:  colab attach --label " + s.Label)
						fmt.Println("  • keep polling state anyway:      colab watch --label " + s.Label + " --forever")
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
	cmd.Flags().BoolVar(&stream, "stream", false, "Show the running cell's LIVE stdout (no log file needed; like 'colab attach')")
	cmd.Flags().StringVar(&logPath, "log", "", "Heartbeat mode: tail the last line of this file ON THE RUNTIME each tick (e.g. /content/train.log)")
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
