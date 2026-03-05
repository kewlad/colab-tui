package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/spf13/cobra"
)

func newExecCmd() *cobra.Command {
	var (
		label   string
		noStream bool
		bg      bool
		timeout int
	)
	cmd := &cobra.Command{
		Use:   "exec COMMAND",
		Short: "Run a shell command on the runtime",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			tty, err := runtime.NewTTY(s.BaseURL, s.Token)
			if err != nil {
				return fmt.Errorf("connect TTY: %w", err)
			}
			defer tty.Close()

			command := strings.Join(args, " ")
			dur := time.Duration(timeout) * time.Second

			if bg {
				logPath, err := tty.ExecBackground(command)
				if err != nil {
					return err
				}
				fmt.Printf("Background command started. Log: %s\n", logPath)
				return nil
			}

			if !noStream {
				return tty.ExecStream(command, os.Stdout, dur)
			}

			output, err := tty.Exec(command, dur)
			if err != nil {
				return err
			}
			fmt.Println(output)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "Buffer all output (don't stream)")
	cmd.Flags().BoolVar(&bg, "bg", false, "Run in background")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "Timeout in seconds")
	return cmd
}

func newPythonCmd() *cobra.Command {
	var (
		label    string
		file     string
		noStream bool
		timeout  int
	)
	cmd := &cobra.Command{
		Use:   "python CODE",
		Short: "Execute Python code in the Jupyter kernel",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			var code string
			if file != "" {
				data, err := os.ReadFile(file)
				if err != nil {
					return fmt.Errorf("read file: %w", err)
				}
				code = string(data)
			} else if len(args) > 0 {
				code = strings.Join(args, " ")
			} else {
				// Try reading from stdin (pipe/heredoc)
				stat, _ := os.Stdin.Stat()
				if (stat.Mode() & os.ModeCharDevice) == 0 {
					data, err := io.ReadAll(os.Stdin)
					if err != nil {
						return fmt.Errorf("read stdin: %w", err)
					}
					code = string(data)
				} else {
					return fmt.Errorf("provide code as argument, --file, or pipe via stdin")
				}
			}

			k, err := runtime.NewKernel(s.BaseURL, s.Token)
			if err != nil {
				return fmt.Errorf("connect kernel: %w", err)
			}
			defer k.Close()

			dur := time.Duration(timeout) * time.Second

			if !noStream {
				return k.ExecuteStream(code, os.Stdout, dur)
			}

			output, err := k.Execute(code, dur)
			if err != nil {
				return err
			}
			if output != "" {
				fmt.Println(output)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().StringVarP(&file, "file", "f", "", "Python file to execute")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "Buffer all output (don't stream)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "Timeout in seconds")
	return cmd
}

func newPsCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List processes on runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			tty, err := runtime.NewTTY(s.BaseURL, s.Token)
			if err != nil {
				return fmt.Errorf("connect TTY: %w", err)
			}
			defer tty.Close()

			output, err := tty.Exec("ps aux", 30*time.Second)
			if err != nil {
				return err
			}
			fmt.Println(output)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	return cmd
}

func newLogsCmd() *cobra.Command {
	var (
		label string
		tail  int
	)
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show background command output",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			tty, err := runtime.NewTTY(s.BaseURL, s.Token)
			if err != nil {
				return fmt.Errorf("connect TTY: %w", err)
			}
			defer tty.Close()

			logCmd := "ls -t /tmp/colab-tui/*.log 2>/dev/null | head -1 | xargs"
			if tail > 0 {
				logCmd += fmt.Sprintf(" tail -n %d", tail)
			} else {
				logCmd += " cat"
			}

			output, err := tty.Exec(logCmd, 30*time.Second)
			if err != nil {
				return err
			}
			fmt.Println(output)
			return nil
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().IntVar(&tail, "tail", 0, "Show last N lines")
	return cmd
}
