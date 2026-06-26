package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"text/tabwriter"

	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/netanelbi/colab-tui/internal/session"
	"github.com/spf13/cobra"
)

// newFsCmd is direct runtime-filesystem access over the Jupyter Contents API —
// list/read/write/delete files on the runtime with no rclone remote or Drive
// consent. Best for small files and quick edits; for bulk data (videos, model
// weights, many files) prefer `colab cp` (rclone, single-network from S3/gdrive).
func newFsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "fs",
		Short: "Directly read/write/list files on the runtime (no rclone)",
		Long: `Direct runtime filesystem access via the Jupyter Contents API — no rclone
remote, no Drive consent. Good for small files and quick edits:
  colab fs ls /content
  colab fs cat /content/run.log
  colab fs put ./local.py /content/local.py
  colab fs get /content/out.csv ./out.csv
  colab fs rm /content/tmp.bin
For bulk data prefer 'colab cp' (rclone, streams straight from S3/gdrive).`,
	}
	cmd.AddCommand(newFsLsCmd(), newFsCatCmd(), newFsGetCmd(), newFsPutCmd(), newFsRmCmd())
	return cmd
}

// withSession resolves the session for an fs subcommand.
func withSession(label string) (*session.Session, error) {
	_, store, err := loadAuthAndStore()
	if err != nil {
		return nil, err
	}
	return resolveSession(store, label)
}

func labelFlag(cmd *cobra.Command, label *string) {
	cmd.Flags().StringVar(label, "label", "", "Runtime label")
}

func newFsLsCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "ls [PATH]",
		Short: "List a directory on the runtime (default /content)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := withSession(label)
			if err != nil {
				return err
			}
			dir := "/content"
			if len(args) > 0 {
				dir = args[0]
			}
			entries, err := runtime.ListDir(s.BaseURL, s.Token, dir)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, e := range entries {
				kind := e.Type
				if e.Type == "directory" {
					kind = "dir"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", kind, humanSize(e.Size), e.Name)
			}
			return w.Flush()
		},
	}
	labelFlag(cmd, &label)
	return cmd
}

func newFsCatCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "cat PATH",
		Short: "Print a file from the runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := withSession(label)
			if err != nil {
				return err
			}
			data, err := runtime.GetFile(s.BaseURL, s.Token, args[0])
			if err != nil {
				return err
			}
			_, err = os.Stdout.Write(data)
			return err
		},
	}
	labelFlag(cmd, &label)
	return cmd
}

func newFsGetCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "get REMOTE [LOCAL]",
		Short: "Download a file from the runtime",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := withSession(label)
			if err != nil {
				return err
			}
			data, err := runtime.GetFile(s.BaseURL, s.Token, args[0])
			if err != nil {
				return err
			}
			local := path.Base(args[0])
			if len(args) > 1 {
				local = args[1]
			}
			if err := os.WriteFile(local, data, 0644); err != nil {
				return err
			}
			fmt.Printf("Downloaded %s:%s -> %s (%s)\n", s.Label, args[0], local, humanSize(int64(len(data))))
			return nil
		},
	}
	labelFlag(cmd, &label)
	return cmd
}

func newFsPutCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "put LOCAL [REMOTE]",
		Short: "Upload a file to the runtime",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := withSession(label)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			remote := "/content/" + filepath.Base(args[0])
			if len(args) > 1 {
				remote = args[1]
			}
			if err := runtime.PutFile(s.BaseURL, s.Token, remote, data); err != nil {
				return err
			}
			fmt.Printf("Uploaded %s -> %s:%s (%s)\n", args[0], s.Label, remote, humanSize(int64(len(data))))
			return nil
		},
	}
	labelFlag(cmd, &label)
	return cmd
}

func newFsRmCmd() *cobra.Command {
	var label string
	cmd := &cobra.Command{
		Use:   "rm PATH",
		Short: "Delete a file or directory on the runtime",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := withSession(label)
			if err != nil {
				return err
			}
			if err := runtime.Delete(s.BaseURL, s.Token, args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted %s:%s\n", s.Label, args[0])
			return nil
		},
	}
	labelFlag(cmd, &label)
	return cmd
}

// humanSize formats a byte count compactly.
func humanSize(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}
