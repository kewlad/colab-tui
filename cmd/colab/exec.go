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
			echoTarget(s.Label)

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
				fmt.Fprintln(os.Stderr, "[note] --bg runs outside the kernel — fine for short/medium work, but a job "+
					"outliving the idle-reap (~2.5h) is killed. For long jobs use: colab python --detach (reap-safe).")
				fmt.Printf("Background command started. Log: %s  (tail it: colab logs --label %s)\n", logPath, s.Label)
				return nil
			}

			if kernelMode {
				out, err := kernelShell(s.BaseURL, s.Token, s.Label, command, dur)
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
			return runtimeShell(s.BaseURL, s.Token, s.Label, command, dur, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().BoolVar(&bg, "bg", false, "Run detached via nohup, OUTSIDE the kernel (not reap-safe past ~2.5h; for long jobs use 'python --detach')")
	cmd.Flags().BoolVar(&kernelMode, "kernel", false, "Force the Jupyter kernel channel instead of the TTY (deterministic robust path; plain exec already auto-falls-back)")
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
		Short: "Execute Python CODE in the kernel (this is NOT the python CLI)",
		Long: `Executes Python CODE inside the runtime's Jupyter kernel — it is NOT the python
executable, so there are no python-CLI args. Common mix-ups:

  colab python 'import torch; print(torch.cuda.is_available())'   # ✅ code
  colab python -f train.py                                        # ✅ run a local .py file
  colab exec 'pip install ultralytics'                            # ✅ pip / shell → use exec
  colab python -m pip install x   /   colab python pip install x  # ❌ that's the shell, use exec

A busy kernel keeps the runtime alive; --detach fires a long cell and returns.`,
		// A python-CLI-style invocation (e.g. 'python -m pip') trips flag parsing —
		// redirect the agent to the right command instead of a bare "unknown flag".
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				if h := pythonMisuseHint(strings.Join(args, " ")); h != "" {
					fmt.Fprintln(os.Stderr, h)
				}
			}
			_, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}
			echoTarget(s.Label)

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

			k, err := runtime.NewKernel(s.BaseURL, s.Token, s.Label)
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
	cmd.Flags().StringVarP(&file, "file", "f", "", "Run a local Python file instead of inline CODE")
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "Buffer all output (don't stream)")
	cmd.Flags().BoolVar(&detach, "detach", false, "Fire the cell and return; the kernel keeps running it (reap-safe way to launch a long job)")
	cmd.Flags().IntVar(&timeout, "timeout", 0, "Timeout in seconds (0 = unlimited)")
	// Turn a python-CLI-style flag (e.g. 'python -m pip') into a helpful redirect.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return fmt.Errorf("%w\n`colab python` runs Python CODE in the kernel, not the python CLI. "+
			"For pip/shell use:  colab exec 'pip install ...'  •  to pass code:  colab python 'print(1)'", err)
	})
	return cmd
}

// pythonMisuseHint returns a redirect hint when CODE looks like a shell/CLI
// command (the recurring "treated colab python as the python binary" mistake),
// or "" if it looks like real Python. Non-blocking — the caller still runs it.
func pythonMisuseHint(code string) string {
	fields := strings.Fields(strings.TrimSpace(code))
	if len(fields) == 0 {
		return ""
	}
	shell := map[string]bool{
		"pip": true, "pip3": true, "python": true, "python3": true, "apt": true,
		"apt-get": true, "conda": true, "sudo": true, "cd": true, "ls": true,
		"mkdir": true, "rm": true, "wget": true, "curl": true, "git": true,
		"nvidia-smi": true, "bash": true, "sh": true, "export": true, "source": true,
	}
	if shell[fields[0]] {
		return fmt.Sprintf("hint: `colab python` runs Python CODE in the kernel, not the shell. For that command use:\n      colab exec %q", strings.TrimSpace(code))
	}
	if len(fields) == 1 && strings.HasSuffix(fields[0], ".py") {
		return fmt.Sprintf("hint: `colab python` takes CODE, not a filename. To run a local file:  colab python -f %s", fields[0])
	}
	return ""
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
