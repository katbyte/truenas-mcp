//go:build acceptance

package truenasrpc_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/katbyte/truenas-mcp/acceptance/layout"
	"github.com/katbyte/truenas-mcp/sdk/truenasrpc"
)

// snapshotsOf is the names of the snapshots a dataset has.
func snapshotsOf(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, dataset string) []string {
	t.Helper()

	var found []struct {
		Name string `json:"name"`
	}
	if err := conn.Call(ctx, "pool.snapshot.query", []any{[]any{[]any{"dataset", "=", dataset}}}, &found); err != nil {
		t.Fatalf("listing the snapshots of %s: %v", dataset, err)
	}

	names := make([]string, len(found))
	for i, one := range found {
		names[i] = one.Name
	}

	return names
}

// asked calls a read with arguments and holds its answer to the description, which is how a method only a journey can reach gets the check
// every plain read gets.
func asked(ctx context.Context, t *testing.T, conn *truenasrpc.Conn, method string, params []any, into any) {
	t.Helper()

	var answer json.RawMessage
	if err := conn.Call(ctx, method, params, &answer); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	heldTo(t, description(t)[method], answer, true) //nolint:contextcheck // the description is read from the repository, and setting the NAS up, which it may wait on, has a limit of its own

	if into != nil {
		if err := json.Unmarshal(answer, into); err != nil {
			t.Fatalf("%s answered %.200s, which does not read: %v", method, answer, err)
		}
	}
}

// A replication keeps a copy of a dataset up to date. The first run copies everything, and a run after the dataset has changed copies what
// changed: what the copy holds is read each time. Around it, everything a replication is asked before one is set up is asked, a copy is made
// once without a task, the task is switched off, and the task that would bring the copy back is made.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestAReplicationCopiesADatasetAndThenOnlyWhatChanged(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 15*time.Minute)

	source, copies := layout.Pool+"/source", layout.Pool+"/copies"
	target, schema := copies+"/source", "kept-%Y-%m-%d_%H-%M"
	gone(ctx, t, conn, source, copies, layout.Pool+"/restored")

	for _, dataset := range []string{source, copies} {
		if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
			t.Fatalf("making %s: %v", dataset, err)
		}
	}
	put(ctx, t, conn, "/mnt/"+source+"/note.txt", "as it was the first day\n")

	snapshots := madeOne(ctx, t, conn, "pool.snapshottask", map[string]any{"dataset": source, "naming_schema": schema, "lifetime_value": 1, "lifetime_unit": "HOUR"})
	copying := madeOne(ctx, t, conn, "replication", map[string]any{"name": "a copy kept up to date", "direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{source}, "target_dataset": target, "recursive": false, "auto": false, "also_include_naming_schema": []string{schema}, "retention_policy": "SOURCE", "readonly": "SET"})

	// what a NAS offers to be copied, and the names it knows snapshots by
	var offered, schemas []string
	asked(ctx, t, conn, "replication.list_datasets", []any{"LOCAL"}, &offered)
	asked(ctx, t, conn, "replication.list_naming_schemas", nil, &schemas)
	if !slices.Contains(offered, source) || !slices.Contains(schemas, schema) {
		t.Errorf("the dataset is not among those offered to copy, or its snapshots' naming not among those known: %v, %v", offered, schemas)
	}

	// a snapshot is taken and the copy brought up to it, and the copy then holds what the dataset held
	round := func(taken int, holds string) {
		if err := conn.Call(ctx, "pool.snapshottask.run", []any{snapshots}, nil); err != nil {
			t.Fatalf("taking a snapshot: %v", err)
		}
		if !eventually(ctx, 2*time.Minute, func() bool { return len(snapshotsOf(ctx, t, conn, source)) == taken }) {
			t.Fatalf("the dataset has %d snapshots, want %d", len(snapshotsOf(ctx, t, conn, source)), taken)
		}

		if err := conn.Call(ctx, "replication.run", []any{copying}, nil); err != nil {
			t.Fatalf("bringing the copy up to date: %v", err)
		}
		if state, why := task(ctx, t, conn, "replication.query", copying).state(); state != "FINISHED" {
			t.Errorf("the replication says %s (%s) after a run that worked", state, why)
		}
		if copied := snapshotsOf(ctx, t, conn, target); len(copied) != taken {
			t.Errorf("the copy has %d snapshots, want %d: %v", len(copied), taken, copied)
		}
		if copied := got(ctx, t, conn, "/mnt/"+target+"/note.txt"); copied != holds {
			t.Errorf("the copy holds %q, want %q", copied, holds)
		}
	}
	round(1, "as it was the first day\n")

	// snapshots are named by the minute, so the next is taken in the next
	time.Sleep(time.Duration(62-time.Now().Second()) * time.Second)
	put(ctx, t, conn, "/mnt/"+source+"/note.txt", "as it was the second day\n")
	round(2, "as it was the second day\n")

	// nothing is on the copy that is not on the dataset, and both snapshots could have been copied by hand
	var unmatched map[string]json.RawMessage
	asked(ctx, t, conn, "replication.target_unmatched_snapshots", []any{"PUSH", []string{source}, target, "LOCAL"}, &unmatched)
	t.Logf("snapshots on the copy that the dataset does not have: %v", unmatched)

	var eligible struct {
		Total    int `json:"total"`
		Eligible int `json:"eligible"`
	}
	asked(ctx, t, conn, "replication.count_eligible_manual_snapshots", []any{map[string]any{"datasets": []string{source}, "naming_schema": []string{schema}, "transport": "LOCAL"}}, &eligible)
	if eligible.Total != 2 || eligible.Eligible != 2 {
		t.Errorf("%d of the dataset's %d snapshots are said to be there to copy, want both", eligible.Eligible, eligible.Total)
	}

	// a copy made once, with no task kept for it
	if err := conn.Call(ctx, "replication.run_onetime", []any{map[string]any{"direction": "PUSH", "transport": "LOCAL", "source_datasets": []string{source}, "target_dataset": copies + "/once", "recursive": false, "also_include_naming_schema": []string{schema}, "retention_policy": "NONE", "readonly": "SET"}}, nil); err != nil {
		t.Fatalf("copying the dataset once: %v", err)
	}
	if once := got(ctx, t, conn, "/mnt/"+copies+"/once/note.txt"); once != "as it was the second day\n" {
		t.Errorf("the copy made once holds %q", once)
	}

	var off struct {
		Enabled bool `json:"enabled"`
	}
	if err := conn.Call(ctx, "replication.update", []any{copying, map[string]any{"enabled": false}}, &off); err != nil || off.Enabled {
		t.Errorf("switched off, the replication reads as enabled %t (%v)", off.Enabled, err)
	}

	// the task that would bring the copy back the other way
	var back struct {
		ID      int64    `json:"id"`
		Sources []string `json:"source_datasets"`
		Target  string   `json:"target_dataset"`
	}
	if err := conn.Call(ctx, "replication.restore", []any{copying, map[string]any{"name": "the copy brought back", "target_dataset": layout.Pool + "/restored"}}, &back); err != nil {
		t.Fatalf("making the task that brings the copy back: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Call(context.WithoutCancel(ctx), "replication.delete", []any{back.ID}, nil); err != nil {
			t.Errorf("removing the task that brings the copy back: %v", err)
		}
	})
	if back.ID == copying || !slices.Contains(back.Sources, target) || back.Target != layout.Pool+"/restored" {
		t.Errorf("the task that brings the copy back copies %v to %s; want the copy, to %s/restored", back.Sources, back.Target, layout.Pool)
	}
}

// A task that takes snapshots can be asked, before it is changed or removed, which snapshots the change would let go of. It is then changed,
// and every snapshot it took is removed in one go.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestASnapshotTaskIsChangedSaysWhatAChangeWouldRemoveAndItsSnapshotsGoAtOnce(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	dataset := layout.Pool + "/snapped"
	gone(ctx, t, conn, dataset)
	if err := conn.Call(ctx, "pool.dataset.create", []any{map[string]any{"name": dataset}}, nil); err != nil {
		t.Fatalf("making %s: %v", dataset, err)
	}

	id := madeOne(ctx, t, conn, "pool.snapshottask", map[string]any{"dataset": dataset, "naming_schema": "long-%Y-%m-%d_%H-%M", "lifetime_value": 2, "lifetime_unit": "WEEK"})
	if err := conn.Call(ctx, "pool.snapshottask.run", []any{id}, nil); err != nil {
		t.Fatalf("running the task: %v", err)
	}
	if !eventually(ctx, 2*time.Minute, func() bool { return len(snapshotsOf(ctx, t, conn, dataset)) == 1 }) {
		t.Fatalf("the task took %d snapshots, want 1", len(snapshotsOf(ctx, t, conn, dataset)))
	}

	var onChange, onRemoval map[string][]string
	asked(ctx, t, conn, "pool.snapshottask.update_will_change_retention_for", []any{id, map[string]any{"naming_schema": "short-%Y-%m-%d_%H-%M"}}, &onChange)
	asked(ctx, t, conn, "pool.snapshottask.delete_will_change_retention_for", []any{id}, &onRemoval)
	// with one snapshot, taken a moment ago and to be kept for weeks either way, neither lets go of anything: seen on a real NAS
	if len(onChange) != 0 || len(onRemoval) != 0 {
		t.Errorf("renaming what the task takes is said to let go of %v, and removing the task of %v; a real NAS said neither would let go of anything", onChange, onRemoval)
	}

	var changed struct {
		Value   int    `json:"lifetime_value"`
		Unit    string `json:"lifetime_unit"`
		Enabled bool   `json:"enabled"`
	}
	if err := conn.Call(ctx, "pool.snapshottask.update", []any{id, map[string]any{"lifetime_value": 3, "lifetime_unit": "DAY", "enabled": false}}, &changed); err != nil || changed.Value != 3 || changed.Unit != "DAY" || changed.Enabled {
		t.Errorf("changed, the task reads as %+v (%v)", changed, err)
	}

	var most int
	asked(ctx, t, conn, "pool.snapshottask.max_count", nil, &most)
	if most <= 0 {
		t.Errorf("the most snapshots a dataset may have is said to be %d", most)
	}

	var removed []string
	if err := conn.Call(ctx, "pool.dataset.destroy_snapshots", []any{dataset, map[string]any{"all": true}}, &removed); err != nil || len(removed) != 1 {
		t.Errorf("removing every snapshot of the dataset removed %v (%v), want the one", removed, err)
	}
	if left := snapshotsOf(ctx, t, conn, dataset); len(left) != 0 {
		t.Errorf("every snapshot removed, the dataset has %v", left)
	}
}

// The system a NAS starts from is kept as boot environments, so that an update can be gone back on. One is copied, marked to be kept, and
// removed, and the one the NAS is running stays the one it will start from.
//
//nolint:paralleltest // the tests share one NAS, which scrubs a pool one job at a time
func TestABootEnvironmentIsCopiedKeptAndRemoved(t *testing.T) {
	conn := admin(t)
	ctx := within(t, 8*time.Minute)

	type environment struct {
		ID        string `json:"id"`
		Active    bool   `json:"active"`
		Activated bool   `json:"activated"`
		Keep      bool   `json:"keep"`
	}
	environments := func() []environment {
		var found []environment
		if err := conn.Call(ctx, "boot.environment.query", nil, &found); err != nil {
			t.Fatalf("listing the boot environments: %v", err)
		}

		return found
	}

	before := environments()
	running := slices.IndexFunc(before, func(one environment) bool { return one.Active })
	if running < 0 {
		t.Fatalf("none of the %d boot environments is the one running", len(before))
	}

	const name = "copied-by-the-tests"
	var copied environment
	if err := conn.Call(ctx, "boot.environment.clone", []any{map[string]any{"id": before[running].ID, "target": name}}, &copied); err != nil || copied.ID != name || copied.Active {
		t.Fatalf("copying the running boot environment made %+v (%v)", copied, err)
	}
	removed := false
	t.Cleanup(func() {
		if !removed {
			_ = conn.Call(context.WithoutCancel(ctx), "boot.environment.destroy", []any{map[string]any{"id": name}}, nil)
		}
	})

	var kept environment
	if err := conn.Call(ctx, "boot.environment.keep", []any{map[string]any{"id": name, "value": true}}, &kept); err != nil || !kept.Keep {
		t.Errorf("marked to be kept, the copy reads as %+v (%v)", kept, err)
	}
	if err := conn.Call(ctx, "boot.environment.keep", []any{map[string]any{"id": name, "value": false}}, &kept); err != nil || kept.Keep {
		t.Errorf("no longer marked to be kept, the copy reads as %+v (%v)", kept, err)
	}

	if err := conn.Call(ctx, "boot.environment.destroy", []any{map[string]any{"id": name}}, nil); err != nil {
		t.Fatalf("removing the copy: %v", err)
	}
	removed = true

	after := environments()
	if len(after) != len(before) || slices.ContainsFunc(after, func(one environment) bool { return one.ID == name }) {
		t.Errorf("the copy removed, there are %d boot environments and were %d", len(after), len(before))
	}
	if now := slices.IndexFunc(after, func(one environment) bool { return one.Active && one.Activated }); now < 0 || after[now].ID != before[running].ID {
		t.Error("the boot environment the NAS runs, and will start from, is not the one it was")
	}
}
