package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var verbose bool

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
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
