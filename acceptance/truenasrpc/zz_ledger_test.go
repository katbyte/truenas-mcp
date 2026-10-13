//go:build acceptance

package truenasrpc_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/katbyte/truenas-mcp/acceptance/described"
)

// The last test of the run, by its file's name: it says how much of the API the tests before it reached, and fails if any line of trace in the
// whole run held a key the tests made.
//
//nolint:paralleltest // it reads what every test before it left
func TestTheRunSaysHowMuchOfTheAPIItTriedAndShowedNoKey(t *testing.T) {
	methods := description(t)

	ledger.mu.Lock()
	showed := slices.Clone(ledger.showed)
	ledger.mu.Unlock()

	for _, line := range showed {
		t.Errorf("a key the tests made is in a line of trace: %s", line)
	}

	var tried, total int
	for _, kind := range []described.Kind{described.Read, described.ReadWithArguments, described.ReadJob, described.Write, described.File, described.Session} {
		var untried []string
		names := named(methods, kind)
		for _, name := range names {
			if ledger.took(name) == 0 {
				untried = append(untried, name)
			}
		}

		tried, total = tried+len(names)-len(untried), total+len(names)
		t.Logf("%s: %d of %d tried; not yet: %s", kind, len(names)-len(untried), len(names), strings.Join(untried, " "))
	}

	t.Logf("%d of the %d methods the NAS has were called in this run", tried, total)
}
