package cmd

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/nerdneilsfield/go-template/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			loggerFrom(cmd).Debug("printing version")
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"go-template %s\ncommit: %s\nbuilt: %s\ngo: %s %s/%s\n",
				buildinfo.Version, buildinfo.Commit, buildinfo.Date,
				runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
