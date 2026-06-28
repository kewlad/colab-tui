package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/netanelbi/colab-tui/internal/auth"
	"github.com/netanelbi/colab-tui/internal/config"
	"github.com/spf13/cobra"
)

func newProfileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profile",
		Short: "Manage account profiles (separate Google accounts)",
		Long: `Each profile is an isolated config tree (tokens, sessions, keepalive
daemons), so different Google accounts run in parallel. Select one with the
global --profile flag or the COLAB_PROFILE environment variable, e.g.

  colab --profile work auth login
  colab --profile work start
  COLAB_PROFILE=personal colab list`,
	}
	cmd.AddCommand(newProfileListCmd(), newProfileUseCmd())
	return cmd
}

func newProfileUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Set the default profile for future commands",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := config.WriteActiveProfile(name); err != nil {
				return err
			}
			fmt.Printf("Default profile set to %q.\n", name)
			// Warn (don't fail) if it has no token yet — `use` then `login` is fine.
			m, _ := auth.Load(filepath.Join(config.ProfilesDir(), name, "auth.json"))
			if m == nil || m.RefreshToken == "" {
				fmt.Printf("Note: %q is not authenticated yet. Run: colab --profile %s auth login\n", name, name)
			}
			return nil
		},
	}
}

func newProfileListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured account profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			entries, err := os.ReadDir(config.ProfilesDir())
			if os.IsNotExist(err) {
				fmt.Println("No profiles yet. Run: colab auth login")
				return nil
			}
			if err != nil {
				return err
			}

			var names []string
			for _, e := range entries {
				if e.IsDir() {
					names = append(names, e.Name())
				}
			}
			if len(names) == 0 {
				fmt.Println("No profiles yet. Run: colab auth login")
				return nil
			}
			sort.Strings(names)

			active := config.Profile()
			for _, name := range names {
				m, _ := auth.Load(filepath.Join(config.ProfilesDir(), name, "auth.json"))
				status := "not authenticated"
				email := ""
				if m != nil && m.RefreshToken != "" {
					status = "authenticated"
					email = m.Email
				}
				marker := " "
				if name == active {
					marker = "*"
				}
				if email != "" {
					fmt.Printf("%s %-14s %s (%s)\n", marker, name, email, status)
				} else {
					fmt.Printf("%s %-14s (%s)\n", marker, name, status)
				}
			}
			return nil
		},
	}
}
