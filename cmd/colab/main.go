package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var verbose bool

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
	}

	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Verbose debug output")

	root.AddCommand(
		newAuthCmd(),
		newStartCmd(),
		newStopCmd(),
		newListCmd(),
		newStatusCmd(),
		newRestartCmd(),
		newResumeCmd(),
		newExecCmd(),
		newPythonCmd(),
		newAttachCmd(),
		newPsCmd(),
		newLogsCmd(),
		newHealthCmd(),
		newQuotaCmd(),
		newResourcesCmd(),
		newKeepaliveCmd(),
		newDriveCmd(),
		newRcloneCmd(),
		newCpCmd(),
		newFsCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
