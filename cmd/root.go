// Package cmd defines the napm command tree.
package cmd

import (
	"context"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/nerdneilsfield/newapi-multi-website-manager/internal/logging"
)

type loggerKey struct{}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:           "napm",
		Short:         "Manage multiple new-api platforms.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			ctx := context.WithValue(cmd.Context(), loggerKey{}, logging.New(verbose))
			cmd.SetContext(ctx)
		},
		PersistentPostRun: func(cmd *cobra.Command, _ []string) {
			_ = loggerFrom(cmd).Sync()
		},
	}

	cmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable debug logging")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newServeCmd())
	return cmd
}

// Execute runs the root command with ctx, which is canceled on SIGINT/SIGTERM.
func Execute(ctx context.Context) error {
	return NewRootCmd().ExecuteContext(ctx)
}

func loggerFrom(cmd *cobra.Command) *zap.Logger {
	if l, ok := cmd.Context().Value(loggerKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.NewNop()
}
