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
		label      string
		bg         bool
		timeout    int
		kernelMode bool
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

			command := strings.Join(args, " ")
			dur := time.Duration(timeout) * time.Second

			if bg {
				tty, err := runtime.NewTTY(s.BaseURL, s.Token)
				if err != nil {
					return fmt.Errorf("connect TTY: %w", err)
				}
				defer tty.Close()
				logPath, err := tty.ExecBackground(command)
				if err != nil {
					return err
				}
				// Persist the log path so `colab logs` tails THIS job's output
				// rather than guessing the most-recent file on the box.
				s.BgLogPath = logPath
				_ = store.Save()
				fmt.Printf("Background command started. Log: %s\n", logPath)
				return nil
			}

			if kernelMode {
				out, err := kernelShell(s.BaseURL, s.Token, command, dur)
				if err != nil {
					return err
				}
				fmt.Print(out)
				if !strings.HasSuffix(out, "\n") {
					fmt.Println()
				}
				return nil
			}

			// TTY, auto-falling back to the kernel channel if the terminal is wedged.
			return runtimeShell(s.BaseURL, s.Token, command, dur, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().BoolVar(&bg, "bg", false, "Run in background")
	cmd.Flags().BoolVar(&kernelMode, "kernel", false, "Run via the Jupyter kernel instead of the TTY (robust if the terminal wedges)")
	cmd.Flags().IntVar(&timeout, "timeout", 300, "Timeout in seconds")
	return cmd
}

func newPythonCmd() *cobra.Command {
	var (
		label    string
		file     string
		noStream bool
		detach   bool
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

			if detach {
				// Fire the cell and return immediately; the kernel keeps executing it
				// (and stays "busy", so Colab won't idle-reap the runtime). Ideal for
				// long-running tasks like training. Monitor via `colab attach` or by
				// reading a log file the cell writes (e.g. `colab exec "tail ..."`).
				if err := k.ExecuteDetach(code); err != nil {
					return err
				}
				fmt.Println("[detached] cell submitted; kernel is running it. Monitor: `colab attach` or `colab exec` on your log file.")
				return nil
			}

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
	cmd.Flags().BoolVar(&detach, "detach", false, "Submit cell and return immediately; kernel keeps running it (keeps runtime alive for long tasks)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Timeout in seconds (0 = unlimited)")
	return cmd
}

func newAttachCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attach to running kernel output (reconnect to ongoing execution)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			k, err := runtime.NewKernelAttach(s.BaseURL, s.Token)
			if err != nil {
				return fmt.Errorf("connect kernel: %w", err)
			}
			defer k.Close()

			return k.Attach(os.Stdout)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
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

			// Prefer the log path recorded by the last `exec --bg` for this
			// session; fall back to the most-recent log on the box otherwise.
			target := "$(ls -t /tmp/colab-tui/*.log 2>/dev/null | head -1)"
			if s.BgLogPath != "" {
				target = s.BgLogPath
			}
			var logCmd string
			if tail > 0 {
				logCmd = fmt.Sprintf("tail -n %d %s", tail, target)
			} else {
				logCmd = "cat " + target
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
