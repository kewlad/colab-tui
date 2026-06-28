package main

import (
	"fmt"
	"os"

	"github.com/netanelbi/colab-tui/internal/config"
	"github.com/spf13/cobra"
)

var (
	verbose bool
	profile string
)

// echoTarget notes which runtime a mutating command is acting on. It writes to
// stderr so it never pollutes captured stdout (e.g. an agent parsing exec output).
func echoTarget(label string) {
	fmt.Fprintf(os.Stderr, "[%s]\n", label)
}

func main() {
	root := &cobra.Command{
		Use:           "colab",
		Short:         "Colab runtime manager & AI agent gateway",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Precedence: --profile flag > COLAB_PROFILE env > persisted
			// active profile > "default".
			if profile == "" {
				profile = os.Getenv("COLAB_PROFILE")
			}
			if profile == "" {
				profile = config.ReadActiveProfile()
			}
			config.SetProfile(profile)
			return config.MigrateLegacy()
		},
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose debug output")
	root.PersistentFlags().StringVar(&profile, "profile", "", `Account profile to use (default: $COLAB_PROFILE or "default")`)

	root.AddCommand(
		newAuthCmd(),
		newStartCmd(),
		newStopCmd(),
		newListCmd(),
		newStatusCmd(),
		newRestartCmd(),
		newInterruptCmd(),
		newResumeCmd(),
		newExecCmd(),
		newPythonCmd(),
		newAttachCmd(),
		newPsCmd(),
		newLogsCmd(),
		newHealthCmd(),
		newQuotaCmd(),
		newResourcesCmd(),
		newWatchCmd(),
		newKeepaliveCmd(),
		newDriveCmd(),
		newRcloneCmd(),
		newCpCmd(),
		newFsCmd(),
		newProfileCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
