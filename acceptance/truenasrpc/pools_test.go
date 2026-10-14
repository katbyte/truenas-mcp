//go:build acceptance

package truenasrpc_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// A NAS with a pool already is given a second one, on a single disk, which is as unsafe as a pool gets: the NAS makes it and calls it healthy,
// so that it rests on one disk is something only its layout says. A second disk is then attached, which turns the one disk into a pair, and
// the pool is removed, which gives both disks back.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASecondPoolIsMadeOnOneDiskGivenASecondAndRemoved(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 20*time.Minute)

	const name = "scratch"
	free := spares(ctx, t, conn)
	if len(free) < 2 {
		t.Fatalf("the NAS has %d disks nothing uses, and a second pool wants two", len(free))
	}

	var made struct {
		ID int64 `json:"id"`
	}
	if err := conn.Call(ctx, "pool.create", []any{map[string]any{"name": name, "topology": map[string]any{"data": []any{map[string]any{"type": "STRIPE", "disks": []string{free[0].Name}}}}}}, &made); err != nil {
		t.Fatalf("making a pool on one disk: %v", err)
	}
	removed := false
	remove := func(ctx context.Context) error {
		return conn.Call(ctx, "pool.export", []any{made.ID, map[string]any{"destroy": true, "cascade": true}}, nil)
	}
	t.Cleanup(func() {
		if !removed {
			if err := remove(context.WithoutCancel(ctx)); err != nil {
				t.Errorf("removing the pool: %v", err)
			}
		}
	})

	alone, found := poolNamed(ctx, t, conn, name)
	if !found || alone.ID != made.ID || !alone.Healthy || alone.Status != "ONLINE" || len(alone.Topology.Data) != 1 {
		t.Fatalf("the new pool reads as %+v", alone)
	}
	if on := alone.Topology.Data[0]; on.Type != "DISK" || on.Disk != free[0].Name || len(on.Children) != 0 {
		t.Errorf("a pool on one disk reads as a %s on %q with %d under it; want the one DISK, %s", on.Type, on.Disk, len(on.Children), free[0].Name)
	}

	// both pools are there, and the first is as it was
	var all []pooled
	if err := conn.Call(ctx, "pool.query", nil, &all); err != nil || len(all) != 2 {
		t.Fatalf("the NAS lists %d pools (%v), want the two", len(all), err)
	}
	if first := pool(ctx, t, conn); !first.Healthy {
		t.Errorf("the first pool reads as %s with a second made beside it", first.Status)
	}

	// it holds a dataset and a file like any other
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name + "/first"}}, nil); err != nil {
		t.Fatalf("making a dataset on the second pool: %v", err)
	}
	put(ctx, t, conn, "/mnt/"+name+"/first/note.txt", "on the second pool\n")
	if back := got(ctx, t, conn, "/mnt/"+name+"/first/note.txt"); back != "on the second pool\n" {
		t.Errorf("the file on the second pool reads back as %q", back)
	}

	if err := conn.Call(ctx, "pool.attach", []any{made.ID, map[string]any{"target_vdev": alone.Topology.Data[0].GUID, "new_disk": free[1].Name}}, nil); err != nil {
		t.Fatalf("attaching a second disk: %v", err)
	}

	var paired pooled
	if !eventually(ctx, 5*time.Minute, func() bool {
		paired, _ = poolNamed(ctx, t, conn, name)

		return paired.Healthy && len(paired.Topology.Data) == 1 && paired.Topology.Data[0].Type == "MIRROR" && len(paired.Topology.Data[0].Children) == 2 && paired.Topology.Data[0].Children[1].Status == "ONLINE"
	}) {
		t.Fatalf("with a second disk attached the pool reads as %+v", paired)
	}
	if back := got(ctx, t, conn, "/mnt/"+name+"/first/note.txt"); back != "on the second pool\n" {
		t.Errorf("with a second disk attached the file reads back as %q", back)
	}

	if err := remove(ctx); err != nil {
		t.Fatalf("removing the pool: %v", err)
	}
	removed = true

	if _, still := poolNamed(ctx, t, conn, name); still {
		t.Error("the pool was removed and is still listed")
	}

	var back []spare
	if !eventually(ctx, 2*time.Minute, func() bool { back = spares(ctx, t, conn); return len(back) == len(free) }) {
		t.Errorf("the pool gone, the NAS has %d disks nothing uses, and had %d", len(back), len(free))
	}
	if !slices.ContainsFunc(back, func(one spare) bool { return one.Name == free[0].Name }) || pool(ctx, t, conn).Name != layout.Pool {
		t.Errorf("the disks given back are %+v, and %s was one of the pool's", back, free[0].Name)
	}
}
