package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/netanelbi/colab-tui/internal/api"
	"github.com/netanelbi/colab-tui/internal/runtime"
	"github.com/spf13/cobra"
)

const driveAuthType = "dfs_ephemeral"

func newDriveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drive",
		Short: "Google Drive integration",
	}
	cmd.AddCommand(newDriveAuthCmd(), newDriveMountCmd())
	return cmd
}

func newDriveAuthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "auth",
		Short: "Authorize Google Drive access for a runtime",
		Long: `Opens a browser for Google Drive consent. This must be done once
per runtime. After consent, 'colab drive mount' works without browser interaction.

Note: 'colab start --gdrive' does this automatically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}

			s := store.GetDefault()
			if s == nil {
				return fmt.Errorf("no active runtime — start one first with: colab start")
			}

			client := api.NewClient(authMgr)
			client.Verbose = verbose

			return doDriveConsent(cmd.Context(), client, s.Endpoint)
		},
	}
}

// doDriveConsent gets the consent URL, opens browser, and polls until credentials
// are propagated. The consent URL comes from dryrun=true; success is detected by
// dryrun=false returning success (it returns HTTP 400 before consent).
func doDriveConsent(ctx context.Context, client *api.Client, endpoint string) error {
	// First check if already authorized (dryrun=false succeeds)
	result, err := client.PropagateCredentials(ctx, endpoint, driveAuthType, false)
	if err == nil && result.Success {
		fmt.Println("Drive already authorized for this runtime.")
		return nil
	}

	// Get the consent URL from dryrun=true
	check, err := client.PropagateCredentials(ctx, endpoint, driveAuthType, true)
	if err != nil {
		return fmt.Errorf("get consent URL: %w", err)
	}
	if check.UnauthorizedRedirect == "" {
		return fmt.Errorf("no consent URL returned — unexpected state")
	}

	fmt.Println("Opening browser for Google Drive consent...")
	if err := openBrowser(check.UnauthorizedRedirect); err != nil {
		fmt.Printf("Could not open browser automatically.\nOpen this URL manually:\n%s\n", check.UnauthorizedRedirect)
	}

	// Poll: dryrun=false returns 400 before consent, success=true after
	fmt.Print("Waiting for consent")
	for i := 0; i < 60; i++ {
		time.Sleep(2 * time.Second)
		fmt.Print(".")

		result, err = client.PropagateCredentials(ctx, endpoint, driveAuthType, false)
		if err != nil {
			continue // 400 = consent not yet given
		}
		if result.Success {
			fmt.Println(" done!")
			return nil
		}
	}

	fmt.Println()
	return fmt.Errorf("timed out waiting for Drive consent — please try again")
}

func newDriveMountCmd() *cobra.Command {
	var (
		label      string
		mountpoint string
		timeout    int
	)
	cmd := &cobra.Command{
		Use:   "mount",
		Short: "Mount Google Drive on the runtime",
		RunE: func(cmd *cobra.Command, args []string) error {
			authMgr, store, err := loadAuthAndStore()
			if err != nil {
				return err
			}
			s, err := resolveSession(store, label)
			if err != nil {
				return err
			}

			client := api.NewClient(authMgr)
			client.Verbose = verbose

			return mountDrive(cmd.Context(), client, s.Endpoint, s.BaseURL, s.Token, mountpoint, time.Duration(timeout)*time.Second)
		},
	}
	cmd.Flags().StringVar(&label, "label", "", "Runtime label")
	cmd.Flags().StringVar(&mountpoint, "mountpoint", "/content/drive", "Mount point path")
	cmd.Flags().IntVar(&timeout, "timeout", 120, "Timeout in seconds")
	return cmd
}

// mountDrive propagates Drive credentials and executes drive.mount() in the kernel.
// If consent hasn't been given for this runtime, it triggers the browser flow.
func mountDrive(ctx context.Context, client *api.Client, endpoint, baseURL, token, mountpoint string, timeout time.Duration) error {
	fmt.Println("Propagating Drive credentials...")

	// Try direct propagation
	result, err := client.PropagateCredentials(ctx, endpoint, driveAuthType, false)
	if err != nil || !result.Success {
		// Consent not given for this runtime — trigger it
		fmt.Println("Drive consent needed for this runtime.")
		if err := doDriveConsent(ctx, client, endpoint); err != nil {
			return err
		}
	}

	// Execute drive.mount() in the kernel
	fmt.Printf("Mounting Drive at %s...\n", mountpoint)
	k, err := runtime.NewKernel(baseURL, token)
	if err != nil {
		return fmt.Errorf("connect kernel: %w", err)
	}
	defer k.Close()

	code := fmt.Sprintf(`from google.colab import drive
drive.mount(%q)`, mountpoint)

	if err := k.ExecuteStream(code, os.Stdout, timeout); err != nil {
		return fmt.Errorf("drive.mount failed: %w", err)
	}

	return nil
}

func openBrowser(url string) error {
	return exec.Command("xdg-open", url).Start()
}
