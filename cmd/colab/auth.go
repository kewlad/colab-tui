package main

import (
	"context"
	"fmt"

	"github.com/netanelbi/colab-tui/internal/auth"
	"github.com/spf13/cobra"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage authentication",
	}
	cmd.AddCommand(newAuthLoginCmd(), newAuthStatusCmd())
	return cmd
}

func newAuthLoginCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate via browser OAuth flow",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := auth.Load()
			if err != nil {
				return err
			}
			fmt.Println("Opening browser for Google OAuth...")
			if err := m.Login(context.Background()); err != nil {
				return err
			}
			fmt.Println("Authenticated successfully. Refresh token saved.")
			return nil
		},
	}
}

func newAuthStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show authentication status",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := auth.Load()
			if err != nil {
				return err
			}
			if m.RefreshToken == "" {
				fmt.Println("Not authenticated. Run: colab auth login")
				return nil
			}
			fmt.Println("Authenticated (refresh token present)")
			// Try to get an access token to verify
			tok, err := m.GetAccessToken(context.Background())
			if err != nil {
				fmt.Printf("Token refresh failed: %v\n", err)
				return nil
			}
			fmt.Printf("Access token: %s...%s\n", tok[:8], tok[len(tok)-4:])
			return nil
		},
	}
}
