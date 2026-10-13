// Package main implements truenas-mcp, an MCP server and CLI for looking after a TrueNAS.
package main

import (
	"os"

	"github.com/gookit/color"
	"github.com/katbyte/go-kt/clog"
	"github.com/katbyte/truenas-mcp/cli"
)

func main() {
	// the log level comes from TRUENAS_LOG; read it once here, before anything logs
	clog.SetLevelFromEnv("TRUENAS_LOG")

	cmd, err := cli.Make()
	if err != nil {
		clog.Log.Error(color.Sprintf("<red>truenas-mcp: building cmd</> %v", err))

		os.Exit(1)
	}

	if err := cmd.Execute(); err != nil {
		clog.Log.Error(color.Sprintf("<red>truenas-mcp:</> %v", err))

		os.Exit(1)
	}

	os.Exit(0)
}
