package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "colab",
		Short: "Colab runtime manager & AI agent gateway",
	}

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
		newPsCmd(),
		newLogsCmd(),
		newKeepaliveCmd(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
