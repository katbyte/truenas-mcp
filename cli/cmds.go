// Package cli implements the truenas-mcp command line interface: the cobra commands and the flag, environment and config file handling.
package cli

import (
	"errors"
	"fmt"

	"github.com/katbyte/go-kt/version"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func ValidateParams(params []string) func(cmd *cobra.Command, args []string) error {
	return func(_ *cobra.Command, _ []string) error {
		for _, p := range params {
			if viper.GetString(p) != "" {
				continue
			}

			return errors.New(p + " parameter can't be empty")
		}

		return nil
	}
}

// connectionGiven refuses to start a command that talks to a TrueNAS without an address and a key, before anything is dialled.
func connectionGiven(cmd *cobra.Command, args []string) error {
	if err := ValidateParams([]string{"server"})(cmd, args); err != nil {
		return err
	}
	// over the NAS's own socket the NAS can see who is asking, and wants no key
	if viper.GetString("token") == "" && viper.GetString("token-file") == "" && !truenasrpc.OverSocket(viper.GetString("server")) {
		return errors.New("token parameter can't be empty (or give token-file)")
	}

	return nil
}

func Make() (*cobra.Command, error) {
	root := &cobra.Command{
		Use:   "truenas-mcp [command]",
		Short: "truenas-mcp is an MCP server and CLI for looking after a TrueNAS",
		Long: `An MCP server (and CLI) for looking after a TrueNAS from an AI client such as Claude Code.
Complete documentation is available at https://github.com/katbyte/truenas-mcp`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Run \"truenas-mcp help\" for more information about available truenas-mcp commands.")

			return nil
		},
	}

	root.AddCommand(&cobra.Command{
		Use:           "version",
		Short:         "Print the version number of truenas-mcp",
		Long:          `Print the version number of truenas-mcp`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "truenas-mcp "+version.Version)
		},
	})

	root.AddCommand(&cobra.Command{
		Use:           "info",
		Short:         "Check the connection and the key, and print the TrueNAS's version",
		Long:          `Connects to the configured TrueNAS, logs in with the API key and prints the version it runs: the way to find out that the address, the certificate and the key are right.`,
		Args:          cobra.NoArgs,
		PreRunE:       connectionGiven,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			conn, err := GetFlags().NewConn()
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close() }()

			var info struct {
				Version string `json:"version"`
			}
			if err := conn.Call(cmd.Context(), "system.info", nil, &info); err != nil {
				return hinted(err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "TrueNAS %s at %s\n", info.Version, conn.Address())

			return nil
		},
	})

	if err := configureFlags(root); err != nil {
		return nil, fmt.Errorf("unable to configure flags: %w", err)
	}

	return root, nil
}
