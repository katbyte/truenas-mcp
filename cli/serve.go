package cli

import (
	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/go-kt/mcp/server"
	"github.com/katbyte/go-kt/version"
	"github.com/katbyte/truenas-mcp/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

func serveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server (stdio by default, HTTP with --listen)",
		Long: `Runs the MCP server for AI clients such as Claude Code.

Without --listen it speaks MCP over stdio: register it in .mcp.json with the TRUENAS_* environment variables set. With --listen (TRUENAS_LISTEN,
e.g. :8080) it serves the Streamable HTTP transport at /mcp instead, for an always-on deployment. --auth-token (TRUENAS_AUTH_TOKEN) is then required,
and clients must send "Authorization: Bearer <token>"; to serve with no token at all, which lets anyone who can reach the port use every tool, say so
with --allow-no-auth (TRUENAS_ALLOW_NO_AUTH=true).

Every tool only reads unless --enable-write (TRUENAS_ENABLE_WRITE=true) is given, and none deletes without --enable-delete beside it.

The NAS is not called at start: one that is down is something the tools report, not a reason to have no tools.`,
		Args: cobra.NoArgs,
		// the version comes first, so that a start that is refused still says what refused it
		PreRunE: func(cmd *cobra.Command, args []string) error {
			announce("truenas-mcp %s starting", versionLine())

			return connectionGiven(cmd, args)
		},
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true

			f := GetFlags()
			opts, err := f.ToolOptions()
			if err != nil {
				return err
			}
			// an open port is refused before the NAS is asked anything; stdio has no port to guard
			if f.Listen != "" {
				if err := server.CheckAuth(f.serveOptions()); err != nil {
					return err
				}
			}
			nas, err := f.NewConn()
			if err != nil {
				return err
			}
			defer func() { _ = nas.Close() }()

			srv := mcp.NewServer(&mcp.Implementation{
				Name:    "truenas",
				Title:   "TrueNAS",
				Version: version.Version,
			}, &mcp.ServerOptions{Instructions: server.Instructions(tools.Instructions...)})

			registered, err := tools.RegisterAll(srv, nas, opts)
			if err != nil {
				return err
			}
			clog.Log.Infof("registered %d tools for the TrueNAS at %s", len(registered), nas.Address())

			return server.Run(cmd.Context(), srv, f.serveOptions())
		},
	}
}
