package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/netanelbi/colab-tui/internal/session"
	"github.com/spf13/cobra"
)

// sentinelPath is a marker file written to /content when a runtime is started.
// If it's gone, the container was recycled (idle-reaped) — a fresh blank VM is
// now behind the same endpoint and everything under /content was wiped.
const sentinelPath = "/content/.colab-tui"

// writeRuntimeSentinel drops the reap-detection marker on the runtime. Best
// effort — failures are non-fatal (health just reports the marker missing).
func writeRuntimeSentinel(baseURL, token, endpoint string) {
	k, err := runtime.NewKernel(baseURL, token)
	if err != nil {
		return
	}
	defer k.Close()
	code := fmt.Sprintf("import os; os.makedirs('/content', exist_ok=True); open(%q,'w').write(%q)",
		sentinelPath, endpoint)
	_, _ = k.Execute(code, 30*time.Second)
}

// healthProbeCode is the single cell run on the runtime to gather liveness
// signals. The sentinel path is injected as a Python string literal (%q).
func healthProbeCode() string {
	return fmt.Sprintf(`import os, socket, subprocess
print("HOST", socket.gethostname())
print("SENTINEL", os.path.exists(%q))
print("CONTENTN", len(os.listdir("/content")) if os.path.isdir("/content") else -1)
try:
    g = subprocess.run(["nvidia-smi","--query-gpu=name,memory.used,memory.total","--format=csv,noheader"],
                       capture_output=True, text=True, timeout=10).stdout.strip()
except Exception:
    g = "(no GPU)"
print("GPU", g or "(no GPU)")`, sentinelPath)
}

func newHealthCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "health [LABEL]",
		Short: "One-shot liveness & reap check for a runtime (for agents)",
		Long: `Reports, in one call, everything an agent needs to decide if a runtime is
usable: keepalive status, token TTL, kernel state (busy/idle/none), whether the
container was idle-reaped (the /content sentinel is gone), how many files remain
under /content, and the GPU. Use this instead of hand-decoding 'ps | grep
colab_kernel'.`,
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
			client.Verbose = verbose

			fmt.Printf("Label:       %s\n", s.Label)
			fmt.Printf("Endpoint:    %s\n", s.Endpoint)
			fmt.Printf("Accelerator: %s %s\n", s.Variant, s.Accelerator)

			kaStatus := "DEAD (run: colab resume)"
			if session.IsKeepaliveRunning(s.Endpoint) {
				kaStatus = fmt.Sprintf("alive (PID %d)", s.KeepalivePID)
			}
			fmt.Printf("Keepalive:   %s\n", kaStatus)

			if ttl := time.Until(s.TokenExpiry); ttl > 0 {
				fmt.Printf("Token TTL:   %s\n", ttl.Round(time.Second))
			} else {
				fmt.Printf("Token TTL:   expired (run: colab resume)\n")
			}

			// Server-side truth #1 (infra): is this endpoint still assigned to me?
			// This is authoritative — independent of our local file.
			if assigned, ok := assignmentFor(client, s.Endpoint); ok {
				fmt.Printf("Assignment:  active server-side (%s %s %s)\n",
					assigned.Variant, assigned.Accelerator, assigned.MachineShape)
			} else {
				fmt.Printf("Assignment:  NOT FOUND server-side\n")
				fmt.Printf("\nVERDICT: DOWN — Colab no longer lists this endpoint as assigned to you.\n")
				fmt.Printf("Start a fresh one: colab start --gpu --accelerator T4 --rclone <remote>\n")
				return nil
			}

			// Server-side truth #2 (container): kernel presence + the /content
			// sentinel detect a recycle behind a still-assigned endpoint.
			_, state, err := runtime.FirstKernelState(s.BaseURL, s.Token)
			if err != nil {
				fmt.Printf("Kernel:      NONE\n")
				fmt.Printf("\nVERDICT: DOWN — assigned but no kernel reachable (container recycled or\n")
				fmt.Printf("never started). Run: colab resume, or start fresh.\n")
				return nil
			}
			fmt.Printf("Kernel:      %s\n", state)

			// Run the probe to detect a reap (sentinel gone) and report GPU/files.
			k, err := runtime.NewKernel(s.BaseURL, s.Token)
			if err != nil {
				fmt.Printf("Probe:       failed to connect kernel: %v\n", err)
				return nil
			}
			defer k.Close()
			out, err := k.Execute(healthProbeCode(), 30*time.Second)
			if err != nil {
				fmt.Printf("Probe:       %v\n", err)
				return nil
			}
			p := parseProbe(out)
			fmt.Printf("Host:        %s\n", p["HOST"])
			fmt.Printf("GPU:         %s\n", p["GPU"])
			fmt.Printf("/content:    %s files\n", p["CONTENTN"])

			sentinelPresent := p["SENTINEL"] == "True"
			fmt.Println()
			switch {
			case !sentinelPresent && s.SentinelSet:
				// We armed reap detection earlier and the marker is now gone →
				// the container was recycled behind the same endpoint.
				fmt.Printf("VERDICT: REAPED — a sentinel we wrote is gone, so this is a FRESH VM behind\n")
				fmt.Printf("the same endpoint; /content was wiped. Re-stage inputs / resume from gdrive.\n")
			case !sentinelPresent:
				// Untracked runtime (e.g. started before this feature) — establish
				// the marker now so future checks can detect a reap. Not an alarm.
				writeRuntimeSentinel(s.BaseURL, s.Token, s.Endpoint)
				s.SentinelSet = true
				_ = store.Save()
				verdict := "IDLE"
				if state == "busy" {
					verdict = "BUSY"
				}
				fmt.Printf("VERDICT: ALIVE & %s — reap tracking wasn't armed for this runtime; armed it now\n", verdict)
				fmt.Printf("(/content has %s files). Re-run health later to detect a reap.\n", p["CONTENTN"])
			case state == "busy":
				if !s.SentinelSet {
					s.SentinelSet = true
					_ = store.Save()
				}
				fmt.Printf("VERDICT: ALIVE & BUSY — a cell is running (this is what keeps it alive).\n")
				fmt.Printf("A new `colab python` will QUEUE behind it; monitor via `colab exec`/logs.\n")
			default:
				if !s.SentinelSet {
					s.SentinelSet = true
					_ = store.Save()
				}
				fmt.Printf("VERDICT: ALIVE & IDLE — ready to use; /content intact.\n")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	return cmd
}

// assignmentFor queries Colab's server-side assignment list and returns the
// assignment matching endpoint, if any. This is the authoritative "is this
// runtime still mine?" check — independent of the local session file. On a query
// error it returns ok=false conservatively (caller treats it as not-found).
func assignmentFor(client *api.Client, endpoint string) (api.Assignment, bool) {
	assignments, err := client.ListAssignments(context.Background())
	if err != nil {
		return api.Assignment{}, false
	}
	for _, a := range assignments {
		if a.Endpoint == endpoint {
			return a, true
		}
	}
	return api.Assignment{}, false
}

// parseProbe turns the "KEY value" lines of the probe output into a map.
func parseProbe(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			m[k] = strings.TrimSpace(v)
		}
	}
	return m
}
