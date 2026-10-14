//go:build acceptance

// Package tools_test runs every MCP tool against a real TrueNAS: the throwaway one .scripts/testenv.sh installs in a virtual machine, a NAS of its
// own and not the one the SDK's live tests set up from nothing. Until there are tools enough to set a NAS up, it is set up through the SDK, the plain
// way acceptance/setup does it. The run fails when a registered tool was never seen to answer, and when a tool says a key it was given.
//
//	make testacc-tools                                                  # a NAS made, set up, tested and removed
//
//	eval "$(make testenv-up)" && go test -tags acceptance ./acceptance/tools/...   # or against one that is up
package tools_test

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }
