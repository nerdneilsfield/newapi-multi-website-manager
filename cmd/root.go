// Package cmd defines the go-template command tree.
package cmd

import (
	"context"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/nerdneilsfield/go-template/internal/logging"
)

type loggerKey struct{}

// NewRootCmd builds the command tree.
func NewRootCmd() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:           "go-template",
		Short:         "go-template is a template for go projects.",
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
