//go:build acceptance

package truenasrpc_test

import (
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// A NAS takes twenty logins a minute from one address and refuses the rest, each after making it wait. That is nowhere in its description,
// and it stopped these tests when each made a connection of its own. It is made to happen here, late in the run so that nothing after it has
// to log in: more logins than it takes are refused as too many, and a connection that was refused gets in by itself once the minute is up,
// since it leaves the NAS alone between tries.
//
//nolint:paralleltest // the tests share one NAS, and this one uses up the logins it will take
func TestMoreLoginsThanANASTakesInAMinuteAreRefusedAndAConnectionWaitsItOut(t *testing.T) {
	ctx := within(t, 8*time.Minute)
	key := truenasrpc.APIKey(layout.ReadOnlyUser, made(t).ReadOnlyKey)

	var taken, refused int
	for range 24 {
		switch err := as(t, key).Connect(ctx); {
		case err == nil:
			taken++
		case truenasrpc.IsTooManyCalls(err):
			refused++
		default:
			t.Fatalf("a login was turned away, and not for being one too many: %v", err)
		}
	}
	t.Logf("of 24 logins in a row, the NAS took %d and refused %d", taken, refused)
	if refused == 0 || taken == 0 || taken > 20 {
		t.Errorf("the NAS took %d logins and refused %d; want it to take no more than 20 and refuse the rest", taken, refused)
	}

	// one connection, asked again and again: it is refused, leaves the NAS alone, and is in once the NAS takes logins again
	conn := as(t, key)
	started := time.Now()
	var last error
	if !eventually(ctx, 3*time.Minute, func() bool { last = conn.Ping(ctx); return last == nil }) {
		t.Fatalf("a connection refused for one login too many never got in: %v", last)
	}
	t.Logf("a connection that was refused got in by itself after %s", time.Since(started).Round(time.Second))
}
