//go:build acceptance

package truenasrpc_test

import (
	"context"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
)

// A snapshot is what a dataset held at one moment, and everything it is for is done to one here: a file is changed after it and brought back
// by rolling the dataset back, the snapshot is copied out as a dataset of its own, held so that it cannot be removed, and removed once
// nothing needs it. What a NAS says when asked to remove one that is held, or that a copy still rests on, is read as well.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASnapshotBringsBackWhatWasThereIsCopiedHeldAndRemoved(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 10*time.Minute)

	name, copied := layout.Pool+"/kept", layout.Pool+"/kept-copy"
	snapshot, note := name+"@before", "/mnt/"+name+"/note.txt"

	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": name}}, nil); err != nil {
		t.Fatalf("making a dataset: %v", err)
	}
	t.Cleanup(func() {
		for _, dataset := range []string{copied, name} {
			if err := conn.Call(context.WithoutCancel(ctx), "pool.dataset.delete", []any{dataset, map[string]any{"recursive": true, "force": true}}, nil); err != nil && dataset == name {
				t.Errorf("removing %s: %v", dataset, err)
			}
		}
	})

	put(ctx, t, conn, note, "as it was\n")

	var taken struct {
		ID string `json:"id"`
	}
	if err := conn.Call(ctx, "pool.snapshot.create", []any{map[string]any{"dataset": name, "name": "before"}}, &taken); err != nil || taken.ID != snapshot {
		t.Fatalf("taking a snapshot: it is %q (%v), want %s", taken.ID, err, snapshot)
	}

	put(ctx, t, conn, note, "as it is now\n")
	if now := got(ctx, t, conn, note); now != "as it is now\n" {
		t.Fatalf("the file changed after the snapshot reads as %q", now)
	}

	listed := func() int {
		var found []struct {
			Name string `json:"name"`
		}
		if err := conn.Call(ctx, "pool.snapshot.query", []any{[]any{[]any{"dataset", "=", name}}}, &found); err != nil {
			t.Fatalf("listing the snapshots: %v", err)
		}

		return len(found)
	}
	if n := listed(); n != 1 {
		t.Errorf("the dataset has %d snapshots, want the one", n)
	}

	// copied out, the snapshot is a dataset that holds what the first held then
	if err := conn.Call(ctx, "pool.snapshot.clone", []any{map[string]any{"snapshot": snapshot, "dataset_dst": copied}}, nil); err != nil {
		t.Fatalf("copying the snapshot out: %v", err)
	}
	if then := got(ctx, t, conn, "/mnt/"+copied+"/note.txt"); then != "as it was\n" {
		t.Errorf("the copy of the snapshot holds %q, want the file as it was", then)
	}

	// a copy rests on its snapshot, so the snapshot cannot go while the copy is there
	err := conn.Call(ctx, "pool.snapshot.delete", []any{snapshot}, nil)
	if err == nil {
		t.Fatal("a snapshot a copy rests on was removed")
	}
	t.Logf("removing a snapshot a copy rests on is refused with: %v", err)

	if err := conn.Call(ctx, "pool.dataset.delete", []any{copied}, nil); err != nil {
		t.Fatalf("removing the copy: %v", err)
	}

	// held, it cannot go either
	if err := conn.Call(ctx, "pool.snapshot.hold", []any{snapshot}, nil); err != nil {
		t.Fatalf("holding the snapshot: %v", err)
	}
	err = conn.Call(ctx, "pool.snapshot.delete", []any{snapshot}, nil)
	if err == nil {
		t.Fatal("a snapshot that is held was removed")
	}
	t.Logf("removing a snapshot that is held is refused with: %v", err)

	if err := conn.Call(ctx, "pool.snapshot.release", []any{snapshot}, nil); err != nil {
		t.Fatalf("letting go of the snapshot: %v", err)
	}

	if err := conn.Call(ctx, "pool.snapshot.rollback", []any{snapshot}, nil); err != nil {
		t.Fatalf("rolling the dataset back: %v", err)
	}
	if back := got(ctx, t, conn, note); back != "as it was\n" {
		t.Errorf("rolled back, the file reads as %q, want it as it was", back)
	}

	if err := conn.Call(ctx, "pool.snapshot.delete", []any{snapshot}, nil); err != nil {
		t.Fatalf("removing the snapshot: %v", err)
	}
	if n := listed(); n != 0 {
		t.Errorf("the snapshot removed, the dataset has %d", n)
	}
}
