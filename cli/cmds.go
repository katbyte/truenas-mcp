// Package cli implements the truenas-mcp command line interface: the cobra commands and the flag, environment and config file handling.
package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
	"github.com/katbyte/truenas-mcp/tools"
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
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "truenas-mcp "+versionLine())
		},
	})

	root.AddCommand(serveCmd())
	root.AddCommand(toolsCmd())

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

// firstSentence trims a tool description to its opening sentence, so the list stays one line per tool. A full stop that ends an abbreviation ends no
// sentence.
func firstSentence(s string) string {
	for from := 0; ; {
		i := strings.Index(s[from:], ". ")
		if i < 0 {
			return s
		}
		end := from + i
		if !endsInAbbreviation(s[:end+1]) {
			return s[:end+1]
		}
		from = end + 2
	}
}

// endsInAbbreviation reports whether a stretch of text ends in a dotted abbreviation rather than a sentence.
func endsInAbbreviation(s string) bool {
	lower := strings.ToLower(s)
	for _, abbr := range []string{"e.g.", "i.e.", "etc.", "vs."} {
		if strings.HasSuffix(lower, abbr) {
			return true
		}
	}

	return false
}

func toolsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "List the tools and toolsets, and what the current flags would register",
		Long: `Lists every tool truenas-mcp would register with the current flags, grouped by toolset, with its kind (read, write or delete) and what it does.

Needs no NAS: it reports what would be registered, not what a NAS accepts.

  truenas-mcp tools                               # the default set (core), reads only
  truenas-mcp tools --toolsets all                # every tool that only reads
  truenas-mcp tools --toolsets all --enable-write # and the ones that change something
  truenas-mcp tools -q                            # names only`,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			out := cmd.OutOrStdout()
			quiet, _ := cmd.Flags().GetBool("quiet")

			f := GetFlags()
			opts, err := f.ToolOptions()
			if err != nil {
				return err
			}
			list, err := tools.Describe(opts)
			if err != nil {
				return err
			}

			if quiet {
				for _, t := range list {
					_, _ = fmt.Fprintln(out, t.Name)
				}

				return nil
			}

			byset := map[string][]tools.ToolInfo{}
			counts := map[string]int{}
			for _, t := range list {
				byset[t.Toolset] = append(byset[t.Toolset], t)
				counts[t.Kind]++
			}

			for _, set := range tools.ToolsetOrder {
				in := byset[set]
				if len(in) == 0 {
					continue
				}
				_, _ = fmt.Fprintf(out, "\n%s (%d)\n", set, len(in))
				for _, t := range in {
					_, _ = fmt.Fprintf(out, "  %-32s %-6s %s\n", t.Name, t.Kind, firstSentence(t.Description))
				}
			}
			_, _ = fmt.Fprintf(out, "\n%d tools: %d read, %d write, %d delete\n", len(list), counts["read"], counts["write"], counts["delete"])
			switch {
			case !f.EnableWrite:
				_, _ = fmt.Fprintln(out, "only tools that read are listed; --enable-write registers the ones that change something, and --enable-delete beside it the ones that delete")
			case !f.EnableDelete:
				_, _ = fmt.Fprintln(out, "delete tools are hidden; --enable-delete registers them")
			default:
			}
			_, _ = fmt.Fprintf(out, "\ntoolsets: all, %s\n", strings.Join(tools.ToolsetNames(), ", "))
			_, _ = fmt.Fprintf(out, "families: %s\n", strings.Join(tools.FamilyNames(), ", "))
			_, _ = fmt.Fprintf(out, "select with --toolsets / TRUENAS_TOOLSETS; core is always included. Default is %s - use --toolsets all for every tool. --allow-tools adds tools to the sets by name, and with no --toolsets registers only the tools it names; --deny-tools takes tools away.\n", strings.Join(DefaultToolsets, ","))

			return nil
		},
	}
	cmd.Flags().BoolP("quiet", "q", false, "print tool names only")

	return cmd
}
