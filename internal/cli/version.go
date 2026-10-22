package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(info BuildInfo) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the tobira version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			version, commit, date := info.Version, info.Commit, info.Date
			if version == "" {
				version = "dev"
			}
			if commit == "" {
				commit = "none"
			}
			if date == "" {
				date = "unknown"
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "tobira %s (%s, %s)\n", version, commit, date)
			return err
		},
	}
}
