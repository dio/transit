package server

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

func newLocalDataCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "localdata",
		Short: "Start embedded Postgres and print the DSN (for psql / local dev)",
		Long: `Starts the embedded Postgres instance that orange server --local uses,
prints the DSN to stdout, and keeps it running until interrupted.

Useful for inspecting data with psql during development:

  # In one terminal:
  orange localdata
  # prints: postgres://... (and stays running)

  # In another terminal:
  psql "$(orange localdata 2>/dev/null)"
  # or capture it:
  DSN=$(orange localdata --print-only)
  psql "$DSN"`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			dsn, cleanup, err := startEmbeddedPG(ctx, true, logger)
			if err != nil {
				return err
			}
			defer cleanup()

			// Print DSN to stdout so it can be captured.
			fmt.Fprintln(cmd.OutOrStdout(), dsn)

			logger.Info("embedded postgres running — press Ctrl+C to stop")
			<-ctx.Done()
			logger.Info("stopping embedded postgres")
			return nil
		},
	}
}
