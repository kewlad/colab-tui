package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
func writeRuntimeSentinel(baseURL, token, endpoint, label string) {
	k, err := runtime.NewKernel(baseURL, token, label)
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
    m = dict((l.split(":")[0], int(l.split(":")[1].split()[0])) for l in open("/proc/meminfo"))
    print("RAM", m["MemTotal"] - m["MemAvailable"], m["MemTotal"])
except Exception:
    pass
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
				// "The runtime says it has no kernels" and "the runtime isn't
				// answering" are different verdicts: the latter is what a
				// mid-recycle / crashed VM looks like — don't call it DOWN yet.
				if !errors.Is(err, runtime.ErrNoKernels) {
					fmt.Printf("Kernel:      UNREACHABLE (%v)\n", err)
					fmt.Printf("\nVERDICT: UNKNOWN — endpoint is assigned but the Jupyter API isn't answering\n")
					fmt.Printf("(proxy blip, or the VM crashed/is being recycled). Re-run `colab health` in ~30s;\n")
					fmt.Printf("if it persists: colab resume, or start fresh.\n")
					return nil
				}
				fmt.Printf("Kernel:      NONE\n")
				fmt.Printf("\nVERDICT: DOWN — assigned but no kernel reachable (container recycled or\n")
				fmt.Printf("never started). Run: colab resume, or start fresh.\n")
				return nil
			}
			fmt.Printf("Kernel:      %s\n", state)

			// A busy kernel can't run our probe cell without queuing behind the
			// running cell — so don't try. Busy means it's clearly alive (and not a
			// reaped fresh VM, which wouldn't have a busy kernel); report and stop.
			if state == "busy" {
				if !s.SentinelSet {
					s.SentinelSet = true
					_ = store.Save()
				}
				fmt.Printf("\nVERDICT: ALIVE & BUSY — a cell is running (this is what keeps it alive).\n")
				fmt.Printf("A new `colab python` QUEUES behind it; monitor via `colab exec`/`colab logs`.\n")
				return nil
			}

			// Idle kernel: run the probe to detect a reap (sentinel gone) + report GPU/files.
			k, err := runtime.NewKernel(s.BaseURL, s.Token, s.Label)
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
			if ram := formatRAM(p["RAM"]); ram != "" {
				fmt.Printf("RAM:         %s\n", ram)
			}
			fmt.Printf("/content:    %s files\n", p["CONTENTN"])
			for _, warn := range memoryWarnings(p["RAM"], p["GPU"]) {
				fmt.Printf("WARNING:     %s\n", warn)
			}

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
				writeRuntimeSentinel(s.BaseURL, s.Token, s.Endpoint, s.Label)
				s.SentinelSet = true
				_ = store.Save()
				fmt.Printf("VERDICT: ALIVE & IDLE — reap tracking wasn't armed for this runtime; armed it now\n")
				fmt.Printf("(/content has %s files). Re-run health later to detect a reap.\n", p["CONTENTN"])
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

// formatRAM turns the probe's "usedKiB totalKiB" into a human line, or "" if the
// probe didn't report RAM.
func formatRAM(ram string) string {
	used, total, ok := parseRAM(ram)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.1f/%.1f GB used (%d%%)", used, total, int(used/total*100))
}

// parseRAM parses "usedKiB totalKiB" into GB values.
func parseRAM(ram string) (usedGB, totalGB float64, ok bool) {
	parts := strings.Fields(ram)
	if len(parts) != 2 {
		return 0, 0, false
	}
	used, err1 := strconv.ParseFloat(parts[0], 64)
	total, err2 := strconv.ParseFloat(parts[1], 64)
	if err1 != nil || err2 != nil || total <= 0 {
		return 0, 0, false
	}
	return used / 1024 / 1024, total / 1024 / 1024, true
}

// memoryWarnings flags RAM / GPU-memory pressure above 90%. This matters more on
// Colab than elsewhere: an OOM doesn't fail politely — it kills the VM, and Colab
// silently swaps a FRESH blank one behind the same endpoint (/content wiped,
// packages gone). health can't predict that after the fact, but it can warn
// while there's still time to checkpoint.
func memoryWarnings(ram, gpu string) []string {
	var warns []string
	if used, total, ok := parseRAM(ram); ok && used/total >= 0.9 {
		warns = append(warns, fmt.Sprintf("RAM %d%% used (%.1f/%.1f GB) — an OOM kills the VM and Colab replaces it "+
			"with a fresh blank one (/content wiped). Checkpoint to gdrive before allocating more.", int(used/total*100), used, total))
	}
	// GPU probe line: "Tesla T4, 13000 MiB, 15360 MiB" (name, memory.used, memory.total)
	if parts := strings.Split(gpu, ","); len(parts) >= 3 {
		used, err1 := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[len(parts)-2]), "MiB")), 64)
		total, err2 := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(parts[len(parts)-1]), "MiB")), 64)
		if err1 == nil && err2 == nil && total > 0 && used/total >= 0.9 {
			warns = append(warns, fmt.Sprintf("GPU memory %d%% used (%.0f/%.0f MiB) — a CUDA OOM can crash the kernel; "+
				"free models (del + torch.cuda.empty_cache()) or colab restart before loading more.", int(used/total*100), used, total))
		}
	}
	return warns
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
