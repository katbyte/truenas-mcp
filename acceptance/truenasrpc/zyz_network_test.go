//go:build acceptance

package truenasrpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// nic is a network interface as a NAS lists it.
type nic struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func nics(ctx context.Context, t *testing.T, conn *truenasrpc.Conn) []nic {
	t.Helper()

	var found []nic
	if err := conn.Call(ctx, "interface.query", nil, &found); err != nil {
		t.Fatalf("listing the network interfaces: %v", err)
	}

	return found
}

// A change to a NAS's network is not made when it is asked for. It waits, the NAS says a change is waiting, and it is made only when told to
// apply, after which the NAS takes it back by itself unless told within a minute that it can still be reached. Here a change is asked for,
// seen to be waiting, and taken back without ever being applied, since applying one to the only way into a test NAS is how to lose it. It is
// the last thing done to the NAS, by its file's name, for the same reason.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAChangeToTheNetworkWaitsToBeAppliedAndIsTakenBack(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	var wired string
	before := nics(ctx, t, conn)
	for _, one := range before {
		if one.Type == "PHYSICAL" {
			wired = one.Name
		}
	}
	if wired == "" {
		t.Fatalf("the NAS lists no network interface of its own among %+v", before)
	}

	waiting := func() bool {
		t.Helper()

		var is bool
		if err := conn.Call(ctx, "interface.has_pending_changes", nil, &is); err != nil {
			t.Fatalf("asking whether a change to the network is waiting: %v", err)
		}

		return is
	}
	if waiting() {
		t.Fatal("a change to the network is waiting before the test has asked for one")
	}

	const name = "vlan7"
	if err := conn.Call(ctx, "interface.create", []any{map[string]any{"name": name, "type": "VLAN", "vlan_parent_interface": wired, "vlan_tag": 7, "description": "asked for by the tests and never applied"}}, nil); err != nil {
		t.Fatalf("asking for a network interface on %s: %v", wired, err)
	}
	taken := false
	back := func(ctx context.Context) {
		if err := conn.Call(ctx, "interface.rollback", nil, nil); err != nil {
			t.Errorf("taking the change back: %v", err)
		}
	}
	t.Cleanup(func() {
		if !taken {
			back(context.WithoutCancel(ctx))
		}
	})

	if !waiting() {
		t.Error("with a network interface asked for, no change is said to be waiting")
	}
	// nothing was applied, so nothing counts down to being taken back
	var left *int
	if err := conn.Call(ctx, "interface.checkin_waiting", nil, &left); err != nil || left != nil {
		t.Errorf("with nothing applied, the NAS says %v seconds are left to confirm it (%v)", left, err)
	}

	listed := false
	for _, one := range nics(ctx, t, conn) {
		listed = listed || one.Name == name
	}
	if !listed {
		t.Error("the interface asked for is not listed while it waits")
	}

	back(ctx)
	taken = true

	// the NAS looks over its network again when a change is taken back, so it is given a moment to answer
	if !eventually(ctx, 2*time.Minute, func() bool {
		var is bool

		return conn.Call(ctx, "interface.has_pending_changes", nil, &is) == nil && !is
	}) {
		t.Fatal("with the change taken back, one is still said to be waiting, or the NAS no longer answers")
	}
	after := nics(ctx, t, conn)
	if len(after) != len(before) {
		t.Errorf("with the change taken back the NAS lists %+v, and before it %+v", after, before)
	}
	if err := reader(t).Call(ctx, "system.info", nil, nil); err != nil {
		t.Errorf("with the change taken back, the NAS no longer answers the user who may only read: %v", err)
	}
}
