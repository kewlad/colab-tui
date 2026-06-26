package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/spf13/cobra"
)

// newQuotaCmd reports the account's compute-unit balance / quota — how much
// budget is left and the current burn rate across all assigned runtimes.
func newQuotaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "quota",
		Short: "Show compute-unit balance, burn rate, and free-tier quota",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			client := api.NewClient(authMgr)
			client.Verbose = verbose

			c, err := client.GetConsumption(context.Background())
			if err != nil {
				return err
			}

			tier := c.SubscriptionTier
			switch tier {
			case "SUBSCRIPTION_TIER_NONE", "":
				tier = "Free"
			case "SUBSCRIPTION_TIER_PRO":
				tier = "Pro"
			case "SUBSCRIPTION_TIER_PRO_PLUS":
				tier = "Pro+"
			}

			fmt.Printf("Tier:          %s\n", tier)
			fmt.Printf("Paid units:    %.2f CCU\n", c.PaidComputeUnits)
			fmt.Printf("Burn rate:     %.2f CCU/hr (total, across %d runtime(s))\n", c.ConsumptionRateHr, c.AssignmentsCount)
			if c.ConsumptionRateHr > 0 && c.PaidComputeUnits > 0 {
				hrs := c.PaidComputeUnits / c.ConsumptionRateHr
				fmt.Printf("Runway:        ~%.1f h at current rate\n", hrs)
			}
			if c.FreeCcuQuota != nil {
				// remainingTokens is a proto Int64 string in milli-CCUs.
				if n, err := strconv.ParseFloat(c.FreeCcuQuota.RemainingTokens, 64); err == nil {
					fmt.Printf("Free quota:    %.2f CCU remaining\n", n/1000)
				}
				if c.FreeCcuQuota.NextRefillTimeSec > 0 {
					t := time.Unix(c.FreeCcuQuota.NextRefillTimeSec, 0)
					fmt.Printf("Next refill:   %s (in %s)\n", t.Format(time.RFC3339), time.Until(t).Round(time.Minute))
				}
			}

			// The consumption endpoint only gives the AGGREGATE rate, so attribute
			// it to the machines actually assigned (label from the local file).
			if assignments, aerr := client.ListAssignments(context.Background()); aerr == nil && len(assignments) > 0 {
				labelByEndpoint := map[string]string{}
				for _, s := range store.List() {
					labelByEndpoint[s.Endpoint] = s.Label
				}
				fmt.Printf("\nAssigned runtimes (each burning its accelerator's rate):\n")
				for _, a := range assignments {
					label := labelByEndpoint[a.Endpoint]
					if label == "" {
						label = "(untracked)"
					}
					fmt.Printf("  - %-12s %s %s %s  %s\n", label, a.Variant, a.Accelerator, a.MachineShape, a.Endpoint)
				}
				fmt.Printf("Tip: `colab stop <label>` an idle runtime to cut the burn; `colab resources <label>` to see if it's working.\n")
			}
			return nil
		},
	}
}

// newResourcesCmd reports RAM / disk / GPU usage on a runtime, read from the
// runtime's structured resource endpoint (cleaner than scraping nvidia-smi/free).
func newResourcesCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:     "resources [LABEL]",
		Aliases: []string{"top"},
		Short:   "Show RAM / disk / GPU usage on a runtime",
		Args:    cobra.MaximumNArgs(1),
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

			r, err := client.GetResources(context.Background(), s.BaseURL, s.Token)
			if err != nil {
				return err
			}

			usedMem := r.Memory.TotalBytes - r.Memory.FreeBytes
			fmt.Printf("RAM:   %s / %s used\n", gib(usedMem), gib(r.Memory.TotalBytes))
			for _, d := range r.Disks {
				fmt.Printf("Disk:  %s / %s used (%s)\n",
					gib(d.Filesystem.UsedBytes), gib(d.Filesystem.TotalBytes), d.Filesystem.Label)
			}
			if len(r.GPUs) == 0 {
				fmt.Println("GPU:   none")
			}
			for _, g := range r.GPUs {
				fmt.Printf("GPU:   %s — %s / %s mem, %.0f%% util\n",
					g.Name, gib(g.MemoryUsedBytes), gib(g.MemoryTotalBytes), g.GPUUtilization*100)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	return cmd
}

// gib formats a byte count as a GiB string.
func gib(bytes float64) string {
	return fmt.Sprintf("%.1f GiB", bytes/(1024*1024*1024))
}
